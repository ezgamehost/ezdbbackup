package storage

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const (
	defaultMultipartThreshold int64 = 8 << 20
	defaultPartSize           int64 = 5 << 20
	minimumPartSize           int64 = 5 << 20
	maximumPartSize           int64 = 5 << 30
	maximumPartCount                = 10_000
	// AWS permits 10,000 multipart parts of at most 5 GiB each (48.8 TiB).
	maximumObjectSize   int64 = maximumPartSize * maximumPartCount
	defaultAbortTimeout       = 30 * time.Second
)

type fileUploader struct {
	client             s3API
	multipartThreshold int64
	partSize           int64
	abortTimeout       time.Duration
	metadataFilter     s3MetadataFilter
}

func newFileUploader(client s3API) *fileUploader {
	return &fileUploader{
		client:             client,
		multipartThreshold: defaultMultipartThreshold,
		partSize:           defaultPartSize,
		abortTimeout:       defaultAbortTimeout,
	}
}

func (u *fileUploader) Upload(ctx context.Context, bucket, key string, source io.ReaderAt, size int64) (UploadResult, error) {
	if u == nil || u.client == nil {
		return UploadResult{}, safeS3Error("upload object", errors.New("S3 uploader is not configured"))
	}
	if source == nil {
		return UploadResult{}, u.safeError("read staged object", errors.New("staged object reader is required"))
	}
	if size < 0 || size > maximumObjectSize {
		return UploadResult{}, u.safeError("read staged object", errors.New("staged object size is outside S3 limits"))
	}
	if err := ctx.Err(); err != nil {
		return UploadResult{}, u.safeError("upload object", err)
	}
	threshold := u.multipartThreshold
	if threshold <= 0 {
		threshold = defaultMultipartThreshold
	}
	if size <= threshold {
		return u.putObject(ctx, bucket, key, source, size)
	}
	return u.multipart(ctx, bucket, key, source, size)
}

func (u *fileUploader) putObject(ctx context.Context, bucket, key string, source io.ReaderAt, size int64) (UploadResult, error) {
	output, err := u.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(bucket),
		Key:           aws.String(key),
		Body:          io.NewSectionReader(source, 0, size),
		ContentLength: aws.Int64(size),
	})
	if err != nil {
		return UploadResult{}, u.safeError("put object", err)
	}
	if output == nil {
		return UploadResult{}, u.safeError("put object", errors.New("S3 returned an empty PutObject response"))
	}
	return UploadResult{ETag: safeETag(aws.ToString(output.ETag))}, nil
}

func (u *fileUploader) multipart(ctx context.Context, bucket, key string, source io.ReaderAt, size int64) (UploadResult, error) {
	partSize, partCount, err := multipartPlan(size, u.partSize)
	if err != nil {
		return UploadResult{}, u.safeError("prepare multipart upload", err)
	}
	created, err := u.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return UploadResult{}, u.safeError("create multipart upload", err)
	}
	uploadID := ""
	if created != nil {
		uploadID = aws.ToString(created.UploadId)
	}
	if uploadID == "" {
		return UploadResult{}, u.safeError("create multipart upload response missing upload id", errors.New("S3 returned an empty multipart upload ID"))
	}
	if err := ctx.Err(); err != nil {
		return UploadResult{}, u.abort(ctx, bucket, key, uploadID, u.safeError("upload part", err))
	}

	completed := make([]types.CompletedPart, 0, partCount)
	for index := 0; index < partCount; index++ {
		offset := int64(index) * partSize
		length := min(partSize, size-offset)
		partNumber := int32(index + 1)
		output, uploadErr := u.client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket:        aws.String(bucket),
			Key:           aws.String(key),
			UploadId:      aws.String(uploadID),
			PartNumber:    aws.Int32(partNumber),
			ContentLength: aws.Int64(length),
			Body:          io.NewSectionReader(source, offset, length),
		})
		if uploadErr != nil {
			return UploadResult{}, u.abort(ctx, bucket, key, uploadID, u.safeError("upload part", uploadErr))
		}
		etag := ""
		if output != nil {
			etag = safeETag(aws.ToString(output.ETag))
		}
		if etag == "" {
			primary := u.safeError("upload part", errors.New("S3 returned an empty or unsafe part ETag"))
			return UploadResult{}, u.abort(ctx, bucket, key, uploadID, primary)
		}
		completed = append(completed, types.CompletedPart{ETag: aws.String(etag), PartNumber: aws.Int32(partNumber)})
		if err := ctx.Err(); err != nil {
			return UploadResult{}, u.abort(ctx, bucket, key, uploadID, u.safeError("upload part", err))
		}
	}

	output, err := u.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String(bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: completed,
		},
	})
	if err != nil {
		return UploadResult{}, u.abort(ctx, bucket, key, uploadID, u.safeError("complete multipart upload", err))
	}
	if output == nil {
		return UploadResult{}, u.abort(ctx, bucket, key, uploadID, u.safeError("complete multipart upload", errors.New("S3 returned an empty completion response")))
	}
	return UploadResult{
		Location: safeLocation(aws.ToString(output.Location)),
		ETag:     safeETag(aws.ToString(output.ETag)),
	}, nil
}

func (u *fileUploader) abort(ctx context.Context, bucket, key, uploadID string, primary error) error {
	base := context.WithoutCancel(ctx)
	timeout := u.abortTimeout
	if timeout <= 0 {
		timeout = defaultAbortTimeout
	}
	abortCtx, cancel := context.WithTimeout(base, timeout)
	defer cancel()
	_, err := u.client.AbortMultipartUpload(abortCtx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
	})
	if err == nil {
		return primary
	}
	return errors.Join(primary, u.safeError("abort multipart upload", err))
}

func (u *fileUploader) safeError(operation string, err error) error {
	return safeS3ErrorWithFilter(operation, err, u.metadataFilter)
}

func multipartPlan(size, configuredPartSize int64) (int64, int, error) {
	if size < 0 || size > maximumObjectSize {
		return 0, 0, errors.New("object size is outside S3 multipart limits")
	}
	if size == 0 {
		return minimumPartSize, 0, nil
	}
	partSize := configuredPartSize
	if partSize < minimumPartSize {
		partSize = minimumPartSize
	}
	required := divideRoundUp(size, maximumPartCount)
	if required > partSize {
		partSize = required
	}
	if partSize > maximumPartSize {
		return 0, 0, errors.New("required multipart part size exceeds S3 limit")
	}
	partCount := int(divideRoundUp(size, partSize))
	if partCount < 1 || partCount > maximumPartCount {
		return 0, 0, errors.New("required multipart part count exceeds S3 limit")
	}
	return partSize, partCount, nil
}

func divideRoundUp(value, divisor int64) int64 {
	return value/divisor + boolToInt64(value%divisor != 0)
}

func boolToInt64(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

func safeETag(value string) string {
	if value == "" || len(value) > 1024 || !utf8.ValidString(value) || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return ""
	}
	return value
}

func safeLocation(value string) string {
	if value == "" || len(value) > 2048 || !utf8.ValidString(value) || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return ""
	}
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil {
		return ""
	}
	return value
}
