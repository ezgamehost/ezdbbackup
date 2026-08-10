package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestUploaderUsesPutObjectForSmallBoundedSource(t *testing.T) {
	client := &fakeS3API{putOutput: &s3.PutObjectOutput{ETag: aws.String(`"small-etag"`)}}
	uploader := newFileUploader(client)
	data := []byte("small staged object with ignored suffix")
	want := []byte("small staged object")

	result, err := uploader.Upload(context.Background(), "backups", "daily/object.sql.gz", bytes.NewReader(data), int64(len(want)))
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	if len(client.putInputs) != 1 || len(client.createInputs) != 0 {
		t.Fatalf("calls = put:%d create:%d, want one PutObject", len(client.putInputs), len(client.createInputs))
	}
	if got := client.putBodies[0]; !bytes.Equal(got, want) {
		t.Fatalf("PutObject body = %q, want bounded %q", got, want)
	}
	if got := aws.ToInt64(client.putInputs[0].ContentLength); got != int64(len(want)) {
		t.Fatalf("PutObject content length = %d, want %d", got, len(want))
	}
	if result.ETag != `"small-etag"` {
		t.Fatalf("Upload() ETag = %q", result.ETag)
	}
}

func TestUploaderMultipartUsesOrderedBoundedSeekableParts(t *testing.T) {
	client := &fakeS3API{
		createOutput: &s3.CreateMultipartUploadOutput{UploadId: aws.String("upload-id")},
		completeOutput: &s3.CompleteMultipartUploadOutput{
			ETag: aws.String(`"complete-etag"`), Location: aws.String("https://objects.example/safe"),
		},
	}
	uploader := newFileUploader(client)
	data := make([]byte, 11<<20)
	for index := range data {
		data[index] = byte(index % 251)
	}

	result, err := uploader.Upload(context.Background(), "backups", "large.sql.gz", bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	if len(client.putInputs) != 0 || len(client.createInputs) != 1 || len(client.completeInputs) != 1 || len(client.abortInputs) != 0 {
		t.Fatalf("calls = put:%d create:%d complete:%d abort:%d", len(client.putInputs), len(client.createInputs), len(client.completeInputs), len(client.abortInputs))
	}
	if got := client.partNumbers; !slices.Equal(got, []int32{1, 2, 3}) {
		t.Fatalf("part order = %v, want [1 2 3]", got)
	}
	wantSizes := []int{5 << 20, 5 << 20, 1 << 20}
	offset := 0
	for index, body := range client.partBodies {
		if len(body) != wantSizes[index] || !bytes.Equal(body, data[offset:offset+len(body)]) {
			t.Fatalf("part %d size/content mismatch: got %d want %d", index+1, len(body), wantSizes[index])
		}
		offset += len(body)
	}
	completed := client.completeInputs[0].MultipartUpload.Parts
	if len(completed) != 3 {
		t.Fatalf("completed parts = %d, want 3", len(completed))
	}
	for index, part := range completed {
		if aws.ToInt32(part.PartNumber) != int32(index+1) || aws.ToString(part.ETag) != fmt.Sprintf(`"etag-%d"`, index+1) {
			t.Fatalf("completed part[%d] = %#v", index, part)
		}
	}
	if result.ETag != `"complete-etag"` || result.Location != "https://objects.example/safe" {
		t.Fatalf("Upload() result = %#v", result)
	}
}

func TestMultipartPlanUsesLegalDynamicSizingAndAtMostTenThousandParts(t *testing.T) {
	for _, test := range []struct {
		name         string
		size         int64
		wantPartSize int64
		wantParts    int
	}{
		{
			name:         "one above ten thousand minimum parts",
			size:         (5<<20)*10_000 + 1,
			wantPartSize: 5_242_881,
			wantParts:    10_000,
		},
		{
			name:         "maximum object",
			size:         53_687_091_200_000,
			wantPartSize: 5_368_709_120,
			wantParts:    10_000,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			partSize, partCount, err := multipartPlan(test.size, 5<<20)
			if err != nil {
				t.Fatalf("multipartPlan() error = %v", err)
			}
			if partSize != test.wantPartSize || partCount != test.wantParts {
				t.Fatalf("multipartPlan(%d) = size:%d count:%d, want size:%d count:%d", test.size, partSize, partCount, test.wantPartSize, test.wantParts)
			}
			if (int64(partCount)-1)*partSize >= test.size || int64(partCount)*partSize < test.size {
				t.Fatalf("multipart plan does not cover size exactly: size=%d partSize=%d count=%d", test.size, partSize, partCount)
			}
		})
	}
	for _, size := range []int64{-1, 53_687_091_200_001, math.MaxInt64} {
		if _, _, err := multipartPlan(size, 5<<20); err == nil {
			t.Fatalf("multipartPlan(%d) error = nil", size)
		}
	}
}

// This fails if ceiling division is changed to the overflowing
// (value+divisor-1)/divisor form.
func TestDivideRoundUpDoesNotOverflowAtInt64Boundary(t *testing.T) {
	if got, want := divideRoundUp(math.MaxInt64, 10_000), int64(922_337_203_685_478); got != want {
		t.Fatalf("divideRoundUp(MaxInt64, 10000) = %d, want %d", got, want)
	}
}

func TestUploaderEnforcesMultipartObjectSizeBoundaryBeforeS3DataTransfer(t *testing.T) {
	const maximum = int64(53_687_091_200_000)
	createErr := errors.New("controlled create failure")
	client := &fakeS3API{createErr: createErr}
	uploader := newFileUploader(client)

	_, err := uploader.Upload(context.Background(), "backups", "largest.sql.gz", bytes.NewReader(nil), maximum)
	if !errors.Is(err, createErr) {
		t.Fatalf("Upload(maximum) error = %v, want CreateMultipartUpload cause", err)
	}
	if got := len(client.createInputs); got != 1 {
		t.Fatalf("CreateMultipartUpload(maximum) calls = %d, want 1", got)
	}

	client = &fakeS3API{}
	uploader = newFileUploader(client)
	if _, err := uploader.Upload(context.Background(), "backups", "too-large.sql.gz", bytes.NewReader(nil), maximum+1); err == nil {
		t.Fatal("Upload(maximum+1) error = nil")
	}
	if len(client.putInputs) != 0 || len(client.createInputs) != 0 {
		t.Fatalf("S3 calls for maximum+1 = put:%d create:%d, want none", len(client.putInputs), len(client.createInputs))
	}
}

// This fails if an incomplete CreateMultipartUpload response causes an abort
// request that cannot identify any server-side upload.
func TestUploaderDoesNotAbortWithoutMultipartUploadID(t *testing.T) {
	client := &fakeS3API{createOutput: &s3.CreateMultipartUploadOutput{}}
	uploader := newFileUploader(client)

	_, err := uploader.Upload(context.Background(), "backups", "large.sql.gz", bytes.NewReader(make([]byte, 9<<20)), 9<<20)
	if err == nil {
		t.Fatal("Upload() error = nil")
	}
	if got := err.Error(); !strings.Contains(got, "missing upload id") {
		t.Fatalf("Upload() error = %q, want fixed safe missing-upload-ID reason", got)
	}
	if got := len(client.abortInputs); got != 0 {
		t.Fatalf("AbortMultipartUpload() calls = %d, want none without an upload ID", got)
	}
}

func TestUploaderAbortsEveryPostCreateFailureWithFreshBoundedContext(t *testing.T) {
	primary := &typedUploadError{"PRIMARY_SECRET raw endpoint failure"}
	tests := []struct {
		name      string
		configure func(*fakeS3API, context.CancelFunc)
	}{
		{
			name: "upload part failure",
			configure: func(client *fakeS3API, _ context.CancelFunc) {
				client.partErrors = map[int32]error{1: primary}
			},
		},
		{
			name: "complete failure",
			configure: func(client *fakeS3API, _ context.CancelFunc) {
				client.completeErr = primary
			},
		},
		{
			name: "canceled after create",
			configure: func(client *fakeS3API, cancel context.CancelFunc) {
				client.afterCreate = cancel
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := &fakeS3API{createOutput: &s3.CreateMultipartUploadOutput{UploadId: aws.String("upload-id")}}
			test.configure(client, cancel)
			uploader := newFileUploader(client)

			_, err := uploader.Upload(ctx, "backups", "large.sql.gz", bytes.NewReader(make([]byte, 9<<20)), 9<<20)
			if err == nil {
				t.Fatal("Upload() error = nil")
			}
			if test.name == "canceled after create" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("Upload() error = %v, want context cancellation", err)
				}
			} else if !errors.Is(err, primary) {
				t.Fatalf("Upload() error = %v, want primary cause", err)
			}
			if strings.Contains(err.Error(), "PRIMARY_SECRET") {
				t.Fatalf("Upload() exposed raw primary error: %q", err)
			}
			assertFreshAbort(t, client, "backups", "large.sql.gz", "upload-id")
		})
	}
}

func TestUploaderJoinsCompletionAndAbortFailuresWithoutLoggingRawErrors(t *testing.T) {
	completeErr := &typedUploadError{"COMPLETE_SECRET endpoint body"}
	abortErr := &typedAbortError{"ABORT_SECRET endpoint body"}
	client := &fakeS3API{
		createOutput: &s3.CreateMultipartUploadOutput{UploadId: aws.String("upload-id")},
		completeErr:  completeErr,
		abortErr:     abortErr,
	}
	uploader := newFileUploader(client)

	var logOutput bytes.Buffer
	oldWriter := log.Writer()
	log.SetOutput(&logOutput)
	t.Cleanup(func() { log.SetOutput(oldWriter) })

	_, err := uploader.Upload(context.Background(), "backups", "large.sql.gz", bytes.NewReader(make([]byte, 9<<20)), 9<<20)
	if !errors.Is(err, completeErr) || !errors.Is(err, abortErr) {
		t.Fatalf("Upload() error = %v, want completion and abort causes", err)
	}
	var gotComplete *typedUploadError
	var gotAbort *typedAbortError
	if !errors.As(err, &gotComplete) || gotComplete != completeErr || !errors.As(err, &gotAbort) || gotAbort != abortErr {
		t.Fatalf("Upload() typed causes = complete:%p abort:%p", gotComplete, gotAbort)
	}
	for _, forbidden := range []string{"COMPLETE_SECRET", "ABORT_SECRET", "endpoint body"} {
		if strings.Contains(err.Error(), forbidden) || strings.Contains(logOutput.String(), forbidden) {
			t.Fatalf("raw failure %q escaped: error=%q log=%q", forbidden, err, logOutput.String())
		}
	}
	if logOutput.Len() != 0 {
		t.Fatalf("uploader wrote global log output: %q", logOutput.String())
	}
	assertFreshAbort(t, client, "backups", "large.sql.gz", "upload-id")
}

func TestUploaderAbortsMultipartSourceReadFailureAndPreservesCause(t *testing.T) {
	readErr := errors.New("staged descriptor read failure")
	client := &fakeS3API{createOutput: &s3.CreateMultipartUploadOutput{UploadId: aws.String("upload-id")}}
	uploader := newFileUploader(client)
	_, err := uploader.Upload(context.Background(), "backups", "large.sql.gz", failingReaderAt{err: readErr}, 9<<20)
	if !errors.Is(err, readErr) {
		t.Fatalf("Upload() error = %v, want read cause", err)
	}
	assertFreshAbort(t, client, "backups", "large.sql.gz", "upload-id")
}

// This fails if cancellation that races with a successful UploadPart response
// is ignored and the uploader proceeds to another part or completion.
func TestUploaderObservesCancellationAfterSuccessfulPart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &fakeS3API{
		createOutput: &s3.CreateMultipartUploadOutput{UploadId: aws.String("upload-id")},
		afterPart:    cancel,
	}
	uploader := newFileUploader(client)
	_, err := uploader.Upload(ctx, "backups", "large.sql.gz", bytes.NewReader(make([]byte, 11<<20)), 11<<20)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Upload() error = %v, want context cancellation", err)
	}
	if got := client.partNumbers; !slices.Equal(got, []int32{1}) {
		t.Fatalf("uploaded parts after cancellation = %v, want only first part", got)
	}
	if len(client.completeInputs) != 0 {
		t.Fatalf("CompleteMultipartUpload() calls = %d, want none", len(client.completeInputs))
	}
	assertFreshAbort(t, client, "backups", "large.sql.gz", "upload-id")
}

// This uses the real generated S3 client to prove UploadPart receives a
// rewindable bounded section and the SDK's retry middleware can replay it.
func TestUploaderUploadPartUsesSDKRetry(t *testing.T) {
	var firstPartAttempts int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		switch {
		case request.Method == http.MethodPost && query.Has("uploads"):
			writer.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(writer, `<CreateMultipartUploadResult><Bucket>backups</Bucket><Key>large.sql.gz</Key><UploadId>upload-id</UploadId></CreateMultipartUploadResult>`)
		case request.Method == http.MethodPut && query.Get("uploadId") == "upload-id":
			partNumber, _ := strconv.Atoi(query.Get("partNumber"))
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Errorf("read upload part: %v", err)
			}
			if partNumber == 1 {
				firstPartAttempts++
				if len(body) != 5<<20 {
					t.Errorf("first part body size = %d, want %d", len(body), 5<<20)
				}
				if firstPartAttempts == 1 {
					writer.Header().Set("Content-Type", "application/xml")
					writer.Header().Set("Retry-After", "0")
					writer.WriteHeader(http.StatusServiceUnavailable)
					_, _ = io.WriteString(writer, `<Error><Code>ServiceUnavailable</Code><Message>retry</Message></Error>`)
					return
				}
			}
			writer.Header().Set("ETag", fmt.Sprintf(`"etag-%d"`, partNumber))
			writer.WriteHeader(http.StatusOK)
		case request.Method == http.MethodPost && query.Get("uploadId") == "upload-id":
			writer.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(writer, `<CompleteMultipartUploadResult><Location>http://safe/object</Location><Bucket>backups</Bucket><Key>large.sql.gz</Key><ETag>"complete"</ETag></CompleteMultipartUploadResult>`)
		default:
			http.Error(writer, "unexpected request", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	configuration := aws.Config{
		Region:      "us-east-1",
		Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider("test", "test", "")),
		HTTPClient:  server.Client(),
		Retryer: func() aws.Retryer {
			return retry.NewStandard(func(options *retry.StandardOptions) { options.MaxAttempts = 3 })
		},
	}
	client := s3.NewFromConfig(configuration, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(server.URL)
		options.UsePathStyle = true
	})
	uploader := newFileUploader(client)
	uploader.multipartThreshold = 5 << 20
	data := bytes.Repeat([]byte{0xa5}, (5<<20)+1)

	if _, err := uploader.Upload(context.Background(), "backups", "large.sql.gz", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	if firstPartAttempts != 2 {
		t.Fatalf("first part attempts = %d, want one SDK retry", firstPartAttempts)
	}
}

func assertFreshAbort(t *testing.T, client *fakeS3API, bucket, key, uploadID string) {
	t.Helper()
	if len(client.abortInputs) != 1 {
		t.Fatalf("AbortMultipartUpload() calls = %d, want 1", len(client.abortInputs))
	}
	if client.abortContextErr != nil {
		t.Fatalf("abort context error = %v, want fresh live context", client.abortContextErr)
	}
	if client.abortDeadline.IsZero() {
		t.Fatal("abort context has no deadline")
	}
	remaining := time.Until(client.abortDeadline)
	if remaining < 25*time.Second || remaining > 31*time.Second {
		t.Fatalf("abort deadline remaining = %s, want bounded near 30s", remaining)
	}
	input := client.abortInputs[0]
	if aws.ToString(input.Bucket) != bucket || aws.ToString(input.Key) != key || aws.ToString(input.UploadId) != uploadID {
		t.Fatalf("abort input = %#v", input)
	}
}

type fakeS3API struct {
	putInputs       []*s3.PutObjectInput
	putBodies       [][]byte
	putOutput       *s3.PutObjectOutput
	putErr          error
	createInputs    []*s3.CreateMultipartUploadInput
	createOutput    *s3.CreateMultipartUploadOutput
	createErr       error
	partInputs      []*s3.UploadPartInput
	partNumbers     []int32
	partBodies      [][]byte
	partErrors      map[int32]error
	completeInputs  []*s3.CompleteMultipartUploadInput
	completeOutput  *s3.CompleteMultipartUploadOutput
	completeErr     error
	abortInputs     []*s3.AbortMultipartUploadInput
	abortErr        error
	abortContextErr error
	abortDeadline   time.Time
	afterCreate     context.CancelFunc
	afterPart       context.CancelFunc
}

func (f *fakeS3API) PutObject(_ context.Context, input *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.putInputs = append(f.putInputs, input)
	body, err := io.ReadAll(input.Body)
	f.putBodies = append(f.putBodies, body)
	if err != nil {
		return nil, err
	}
	if f.putErr != nil {
		return nil, f.putErr
	}
	if f.putOutput == nil {
		return &s3.PutObjectOutput{}, nil
	}
	return f.putOutput, nil
}

func (f *fakeS3API) CreateMultipartUpload(_ context.Context, input *s3.CreateMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error) {
	f.createInputs = append(f.createInputs, input)
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.afterCreate != nil {
		f.afterCreate()
	}
	if f.createOutput == nil {
		return &s3.CreateMultipartUploadOutput{UploadId: aws.String("upload-id")}, nil
	}
	return f.createOutput, nil
}

func (f *fakeS3API) UploadPart(_ context.Context, input *s3.UploadPartInput, _ ...func(*s3.Options)) (*s3.UploadPartOutput, error) {
	f.partInputs = append(f.partInputs, input)
	partNumber := aws.ToInt32(input.PartNumber)
	f.partNumbers = append(f.partNumbers, partNumber)
	body, err := io.ReadAll(input.Body)
	f.partBodies = append(f.partBodies, body)
	if err != nil {
		return nil, err
	}
	if err := f.partErrors[partNumber]; err != nil {
		return nil, err
	}
	if f.afterPart != nil {
		f.afterPart()
		f.afterPart = nil
	}
	return &s3.UploadPartOutput{ETag: aws.String(fmt.Sprintf(`"etag-%d"`, partNumber))}, nil
}

func (f *fakeS3API) CompleteMultipartUpload(_ context.Context, input *s3.CompleteMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error) {
	f.completeInputs = append(f.completeInputs, input)
	if f.completeErr != nil {
		return nil, f.completeErr
	}
	if f.completeOutput == nil {
		return &s3.CompleteMultipartUploadOutput{}, nil
	}
	return f.completeOutput, nil
}

func (f *fakeS3API) AbortMultipartUpload(ctx context.Context, input *s3.AbortMultipartUploadInput, _ ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	f.abortInputs = append(f.abortInputs, input)
	f.abortContextErr = ctx.Err()
	f.abortDeadline, _ = ctx.Deadline()
	if f.abortErr != nil {
		return nil, f.abortErr
	}
	return &s3.AbortMultipartUploadOutput{}, nil
}

func (f *fakeS3API) HeadBucket(context.Context, *s3.HeadBucketInput, ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	return &s3.HeadBucketOutput{}, nil
}

type typedUploadError struct{ text string }

func (e *typedUploadError) Error() string { return e.text }

type typedAbortError struct{ text string }

func (e *typedAbortError) Error() string { return e.text }

type failingReaderAt struct{ err error }

func (r failingReaderAt) ReadAt([]byte, int64) (int, error) { return 0, r.err }
