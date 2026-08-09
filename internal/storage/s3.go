package storage

import (
	"context"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type headBucketAPI interface {
	HeadBucket(context.Context, *s3.HeadBucketInput, ...func(*s3.Options)) (*s3.HeadBucketOutput, error)
}

type uploadAPI interface {
	Upload(context.Context, *s3.PutObjectInput, ...func(*manager.Uploader)) (*manager.UploadOutput, error)
}

type AWSFactory struct{}

func (AWSFactory) New(ctx context.Context, opts Options) (Store, error) {
	loadOptions := []func(*config.LoadOptions) error{
		config.WithRegion(opts.Region),
	}
	if opts.Credentials.Explicit {
		loadOptions = append(loadOptions, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(
				opts.Credentials.AccessKeyID,
				opts.Credentials.SecretAccessKey,
				opts.Credentials.SessionToken,
			),
		))
	}

	cfg, err := config.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if opts.Endpoint != "" {
			o.BaseEndpoint = aws.String(opts.Endpoint)
		}
		o.UsePathStyle = opts.ForcePathStyle
	})
	uploader := manager.NewUploader(client, func(u *manager.Uploader) {
		u.LeavePartsOnError = false
	})
	return &s3Store{client: client, uploader: uploader}, nil
}

type s3Store struct {
	client   headBucketAPI
	uploader uploadAPI
}

func (s *s3Store) UploadFile(ctx context.Context, bucket, key, path string) (UploadResult, error) {
	file, err := os.Open(path)
	if err != nil {
		return UploadResult{}, err
	}
	defer file.Close()

	output, err := s.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   file,
	})
	if err != nil {
		return UploadResult{}, err
	}
	return UploadResult{
		Location: output.Location,
		ETag:     aws.ToString(output.ETag),
	}, nil
}

func (s *s3Store) Probe(ctx context.Context, bucket string) error {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)})
	return err
}
