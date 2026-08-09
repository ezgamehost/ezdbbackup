package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type fakeHeadBucketClient struct {
	inputs []*s3.HeadBucketInput
	err    error
}

func (f *fakeHeadBucketClient) HeadBucket(_ context.Context, input *s3.HeadBucketInput, _ ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	f.inputs = append(f.inputs, input)
	if f.err != nil {
		return nil, f.err
	}
	return &s3.HeadBucketOutput{}, nil
}

type fakeUploader struct {
	inputs []*transfermanager.UploadObjectInput
	body   []byte
	file   *os.File
	output *transfermanager.UploadObjectOutput
	err    error
}

func (f *fakeUploader) UploadObject(_ context.Context, input *transfermanager.UploadObjectInput, _ ...func(*transfermanager.Options)) (*transfermanager.UploadObjectOutput, error) {
	f.inputs = append(f.inputs, input)
	f.file, _ = input.Body.(*os.File)
	var readErr error
	f.body, readErr = io.ReadAll(input.Body)
	if readErr != nil {
		return nil, readErr
	}
	return f.output, f.err
}

func TestS3StoreUploadFileUploadsStagedFileAndClosesIt(t *testing.T) {
	path := writeStagedFile(t, "database dump")
	uploader := &fakeUploader{output: &transfermanager.UploadObjectOutput{
		Location: aws.String("https://objects.example/backups/database.sql.gz"),
		ETag:     aws.String(`"etag-value"`),
	}}
	store := &s3Store{uploader: uploader}

	got, err := store.UploadFile(context.Background(), "backups", "mysql/database.sql.gz", path)
	if err != nil {
		t.Fatalf("UploadFile() error = %v", err)
	}
	if len(uploader.inputs) != 1 {
		t.Fatalf("Upload() call count = %d, want 1", len(uploader.inputs))
	}
	if got.Location != "https://objects.example/backups/database.sql.gz" || got.ETag != `"etag-value"` {
		t.Fatalf("UploadFile() = %+v, want returned location and ETag", got)
	}
	input := uploader.inputs[0]
	if got := aws.ToString(input.Bucket); got != "backups" {
		t.Errorf("upload bucket = %q, want %q", got, "backups")
	}
	if got := aws.ToString(input.Key); got != "mysql/database.sql.gz" {
		t.Errorf("upload key = %q, want %q", got, "mysql/database.sql.gz")
	}
	if got := string(uploader.body); got != "database dump" {
		t.Errorf("upload body = %q, want %q", got, "database dump")
	}
	assertFileClosed(t, uploader.file)
}

func TestAWSFactoryPreservesEndpointPathStyleAndCredentialSelection(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "default-access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "default-secret-key")
	t.Setenv("AWS_SESSION_TOKEN", "default-session-token")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	tests := []struct {
		name        string
		credentials Credentials
		wantAccess  string
		wantSecret  string
		wantToken   string
	}{
		{
			name:       "default credential chain",
			wantAccess: "default-access-key",
			wantSecret: "default-secret-key",
			wantToken:  "default-session-token",
		},
		{
			name: "explicit credentials",
			credentials: Credentials{
				AccessKeyID:     "explicit-access-key",
				SecretAccessKey: "explicit-secret-key",
				SessionToken:    "explicit-session-token",
				Explicit:        true,
			},
			wantAccess: "explicit-access-key",
			wantSecret: "explicit-secret-key",
			wantToken:  "explicit-session-token",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, err := (AWSFactory{}).New(context.Background(), Options{
				Region:         "us-east-1",
				Endpoint:       "https://objects.example.test",
				ForcePathStyle: true,
				Credentials:    test.credentials,
			})
			if err != nil {
				t.Fatalf("AWSFactory.New() error = %v", err)
			}

			client, ok := store.(*s3Store).client.(*s3.Client)
			if !ok {
				t.Fatalf("AWSFactory.New() client type = %T, want *s3.Client", store.(*s3Store).client)
			}
			clientOptions := client.Options()
			if got := aws.ToString(clientOptions.BaseEndpoint); got != "https://objects.example.test" {
				t.Errorf("BaseEndpoint = %q, want %q", got, "https://objects.example.test")
			}
			if !clientOptions.UsePathStyle {
				t.Error("UsePathStyle = false, want true")
			}

			gotCredentials, err := clientOptions.Credentials.Retrieve(context.Background())
			if err != nil {
				t.Fatalf("Retrieve() credentials error = %v", err)
			}
			if gotCredentials.AccessKeyID != test.wantAccess ||
				gotCredentials.SecretAccessKey != test.wantSecret ||
				gotCredentials.SessionToken != test.wantToken {
				t.Error("credential provider did not return the expected credential source")
			}
		})
	}
}

type failingMultipartClient struct {
	abortInputs     []*s3.AbortMultipartUploadInput
	abortContextErr error
	cancelUpload    context.CancelFunc
	wantErr         error
}

func (f *failingMultipartClient) PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	panic("unexpected PutObject call")
}

func (f *failingMultipartClient) UploadPart(context.Context, *s3.UploadPartInput, ...func(*s3.Options)) (*s3.UploadPartOutput, error) {
	if f.cancelUpload != nil {
		f.cancelUpload()
	}
	return nil, f.wantErr
}

func (f *failingMultipartClient) CreateMultipartUpload(context.Context, *s3.CreateMultipartUploadInput, ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error) {
	return &s3.CreateMultipartUploadOutput{UploadId: aws.String("upload-id")}, nil
}

func (f *failingMultipartClient) CompleteMultipartUpload(context.Context, *s3.CompleteMultipartUploadInput, ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error) {
	panic("unexpected CompleteMultipartUpload call")
}

func (f *failingMultipartClient) AbortMultipartUpload(ctx context.Context, input *s3.AbortMultipartUploadInput, _ ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	f.abortInputs = append(f.abortInputs, input)
	f.abortContextErr = ctx.Err()
	if f.abortContextErr != nil {
		return nil, f.abortContextErr
	}
	return &s3.AbortMultipartUploadOutput{}, nil
}

func (f *failingMultipartClient) GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	panic("unexpected GetObject call")
}

func (f *failingMultipartClient) HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	panic("unexpected HeadObject call")
}

func (f *failingMultipartClient) ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	panic("unexpected ListObjectsV2 call")
}

func TestTransferManagerAbortsCanceledMultipartUploadWithFreshContext(t *testing.T) {
	wantErr := errors.New("upload part failed")
	ctx, cancel := context.WithCancel(context.Background())
	client := &failingMultipartClient{cancelUpload: cancel, wantErr: wantErr}
	uploader := newTransferManager(client)

	_, err := uploader.UploadObject(ctx, &transfermanager.UploadObjectInput{
		Bucket: aws.String("backups"),
		Key:    aws.String("database.sql.gz"),
		Body:   bytes.NewReader(make([]byte, 6<<20)),
	}, func(options *transfermanager.Options) {
		options.Concurrency = 1
		options.PartSizeBytes = 5 << 20
		options.MultipartUploadThreshold = 5 << 20
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("UploadObject() error = %v, want %v", err, wantErr)
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("upload context error = %v, want %v", ctx.Err(), context.Canceled)
	}
	if len(client.abortInputs) != 1 {
		t.Fatalf("AbortMultipartUpload() call count = %d, want 1", len(client.abortInputs))
	}
	if client.abortContextErr != nil {
		t.Fatalf("AbortMultipartUpload() context error = %v, want nil", client.abortContextErr)
	}
	abortInput := client.abortInputs[0]
	if got := aws.ToString(abortInput.Bucket); got != "backups" {
		t.Errorf("abort bucket = %q, want %q", got, "backups")
	}
	if got := aws.ToString(abortInput.Key); got != "database.sql.gz" {
		t.Errorf("abort key = %q, want %q", got, "database.sql.gz")
	}
	if got := aws.ToString(abortInput.UploadId); got != "upload-id" {
		t.Errorf("abort upload ID = %q, want %q", got, "upload-id")
	}
}

func TestS3StoreUploadFileReturnsUploaderErrorAndClosesFile(t *testing.T) {
	path := writeStagedFile(t, "database dump")
	wantErr := errors.New("upload failed")
	uploader := &fakeUploader{err: wantErr}
	store := &s3Store{uploader: uploader}

	got, err := store.UploadFile(context.Background(), "backups", "database.sql.gz", path)
	if !errors.Is(err, wantErr) {
		t.Fatalf("UploadFile() error = %v, want %v", err, wantErr)
	}
	if got != (UploadResult{}) {
		t.Fatalf("UploadFile() result = %+v, want zero result", got)
	}
	assertFileClosed(t, uploader.file)
}

func TestS3StoreProbeHeadsBucketOnceAndReturnsError(t *testing.T) {
	wantErr := errors.New("head bucket failed")
	client := &fakeHeadBucketClient{err: wantErr}
	store := &s3Store{client: client}

	err := store.Probe(context.Background(), "backups")
	if !errors.Is(err, wantErr) {
		t.Fatalf("Probe() error = %v, want %v", err, wantErr)
	}
	if len(client.inputs) != 1 {
		t.Fatalf("HeadBucket() call count = %d, want 1", len(client.inputs))
	}
	if got := aws.ToString(client.inputs[0].Bucket); got != "backups" {
		t.Fatalf("HeadBucket() bucket = %q, want %q", got, "backups")
	}
}

func writeStagedFile(t *testing.T, contents string) string {
	t.Helper()
	path := t.TempDir() + "/staged.sql.gz"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write staged file: %v", err)
	}
	return path
}

func assertFileClosed(t *testing.T, file *os.File) {
	t.Helper()
	if file == nil {
		t.Fatal("upload body is not an *os.File")
	}
	if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("uploaded file Stat() error = %v, want %v", err, os.ErrClosed)
	}
}
