package storage

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
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
	inputs []*s3.PutObjectInput
	body   []byte
	file   *os.File
	output *manager.UploadOutput
	err    error
}

func (f *fakeUploader) Upload(_ context.Context, input *s3.PutObjectInput, _ ...func(*manager.Uploader)) (*manager.UploadOutput, error) {
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
	uploader := &fakeUploader{output: &manager.UploadOutput{
		Location: "https://objects.example/backups/database.sql.gz",
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
