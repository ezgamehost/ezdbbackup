//go:build integration

package e2e_test

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const (
	integrationRegion = "us-east-1"
	testAccessKey     = "test"
	testSecretKey     = "test"
	wantSQL           = "CREATE TABLE example(id INT);\nINSERT INTO example VALUES (1);\n"
)

func TestBackupToS3(t *testing.T) {
	endpoint := os.Getenv("EZDBBACKUP_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("integration test requires EZDBBACKUP_TEST_S3_ENDPOINT (for example http://127.0.0.1:4566)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)

	repository := repositoryRoot(t)
	temporary := secureIntegrationTempDir(t)
	backupBinary := filepath.Join(temporary, "ezdbbackup")
	fakeDumpBinary := filepath.Join(temporary, "mysqldump-fake")
	buildBinary(t, ctx, repository, backupBinary, "./cmd/ezdbbackup")
	buildBinary(t, ctx, repository, fakeDumpBinary, "./test/fakedump")

	stageDirectory := filepath.Join(temporary, "stage")
	logDirectory := filepath.Join(temporary, "logs")
	for _, directory := range []string{stageDirectory, logDirectory} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatalf("create integration directory %q: %v", directory, err)
		}
	}

	currentUser, err := user.Current()
	if err != nil {
		t.Fatalf("look up current user: %v", err)
	}
	bucket := integrationBucket(temporary)
	client := newS3Client(t, ctx, endpoint)
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("create integration bucket: %v", err)
	}
	t.Cleanup(func() { cleanBucket(t, client, bucket) })

	configPath := filepath.Join(temporary, "config.yml")
	writeConfig(t, configPath, integrationConfig{
		Bucket:         bucket,
		DumpBinary:     fakeDumpBinary,
		Endpoint:       endpoint,
		LogDirectory:   logDirectory,
		RunAs:          currentUser.Username,
		StageDirectory: stageDirectory,
	})

	runCLI(t, ctx, backupBinary, "validate", "--all", "--connectivity", "--config", configPath)
	runCLI(t, ctx, backupBinary, "backup", "production", "--config", configPath)

	objects, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	if err != nil {
		t.Fatalf("list integration objects: %v", err)
	}
	if len(objects.Contents) != 1 {
		t.Fatalf("object count = %d, want exactly 1", len(objects.Contents))
	}
	key := aws.ToString(objects.Contents[0].Key)
	if !strings.HasPrefix(key, "integration/production/") || !strings.HasSuffix(key, ".sql.gz") {
		t.Fatalf("object key = %q, want integration/production/...sql.gz", key)
	}

	object, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("fetch integration object: %v", err)
	}
	compressed, err := io.ReadAll(io.LimitReader(object.Body, 1<<20))
	closeErr := object.Body.Close()
	if err != nil {
		t.Fatalf("read integration object: %v", err)
	}
	if closeErr != nil {
		t.Fatalf("close integration object: %v", closeErr)
	}
	if got := gunzip(t, compressed); got != wantSQL {
		t.Fatalf("decompressed SQL = %q, want %q", got, wantSQL)
	}

	assertJSONLogs(t, logDirectory)
	assertNoStagedBackup(t, stageDirectory)
}

func TestBackupDumpFailureIsLoggedAndCleaned(t *testing.T) {
	endpoint := os.Getenv("EZDBBACKUP_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("integration test requires EZDBBACKUP_TEST_S3_ENDPOINT (for example http://127.0.0.1:4566)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)

	repository := repositoryRoot(t)
	temporary := secureIntegrationTempDir(t)
	backupBinary := filepath.Join(temporary, "ezdbbackup")
	fakeDumpBinary := filepath.Join(temporary, "mysqldump-fake")
	buildBinary(t, ctx, repository, backupBinary, "./cmd/ezdbbackup")
	buildBinary(t, ctx, repository, fakeDumpBinary, "./test/fakedump")

	stageDirectory := filepath.Join(temporary, "stage")
	logDirectory := filepath.Join(temporary, "logs")
	for _, directory := range []string{stageDirectory, logDirectory} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatalf("create integration directory %q: %v", directory, err)
		}
	}

	currentUser, err := user.Current()
	if err != nil {
		t.Fatalf("look up current user: %v", err)
	}
	bucket := integrationBucket(temporary)
	client := newS3Client(t, ctx, endpoint)
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("create integration bucket: %v", err)
	}
	t.Cleanup(func() { cleanBucket(t, client, bucket) })

	const password = "binary-e2e-mysql-secret"
	configPath := filepath.Join(temporary, "config.yml")
	writeConfig(t, configPath, integrationConfig{
		Bucket:         bucket,
		DumpBinary:     fakeDumpBinary,
		Endpoint:       endpoint,
		LogDirectory:   logDirectory,
		Password:       password,
		RunAs:          currentUser.Username,
		StageDirectory: stageDirectory,
	})

	runCLI(t, ctx, backupBinary, "validate", "--all", "--connectivity", "--config", configPath)
	output, exitCode := runCLIWithEnvironment(
		t,
		ctx,
		backupBinary,
		map[string]string{"FAKE_DUMP_FAIL": "1"},
		"backup", "production", "--config", configPath,
	)
	if exitCode != 1 {
		t.Fatalf("backup exit code = %d, want 1\n%s", exitCode, output)
	}
	if bytes.Contains(output, []byte(password)) {
		t.Fatalf("backup output exposed MySQL password: %s", output)
	}

	objects, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	if err != nil {
		t.Fatalf("list integration objects: %v", err)
	}
	if len(objects.Contents) != 0 {
		t.Fatalf("object count after dump failure = %d, want 0", len(objects.Contents))
	}
	assertNoStagedBackup(t, stageDirectory)
	assertDumpFailureLog(t, logDirectory, password)
}

func TestBackupRetriesTransientMultipartUploadAndPreservesObject(t *testing.T) {
	endpoint := os.Getenv("EZDBBACKUP_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("integration test requires EZDBBACKUP_TEST_S3_ENDPOINT (for example http://127.0.0.1:4566)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)

	repository := repositoryRoot(t)
	temporary := secureIntegrationTempDir(t)
	backupBinary := filepath.Join(temporary, "ezdbbackup")
	fakeDumpBinary := filepath.Join(temporary, "mysqldump-fake")
	buildBinary(t, ctx, repository, backupBinary, "./cmd/ezdbbackup")
	buildBinary(t, ctx, repository, fakeDumpBinary, "./test/fakedump")

	stageDirectory := filepath.Join(temporary, "stage")
	logDirectory := filepath.Join(temporary, "logs")
	for _, directory := range []string{stageDirectory, logDirectory} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatalf("create integration directory %q: %v", directory, err)
		}
	}

	currentUser, err := user.Current()
	if err != nil {
		t.Fatalf("look up current user: %v", err)
	}
	bucket := integrationBucket(temporary)
	directClient := newS3Client(t, ctx, endpoint)
	if _, err := directClient.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("create integration bucket: %v", err)
	}
	t.Cleanup(func() { cleanBucket(t, directClient, bucket) })

	proxy := newTransientMultipartUploadProxy(t, endpoint)
	configPath := filepath.Join(temporary, "config.yml")
	writeConfig(t, configPath, integrationConfig{
		Bucket:         bucket,
		DumpBinary:     fakeDumpBinary,
		Endpoint:       proxy.server.URL,
		LogDirectory:   logDirectory,
		RunAs:          currentUser.Username,
		StageDirectory: stageDirectory,
	})

	runCLI(t, ctx, backupBinary, "validate", "--all", "--connectivity", "--config", configPath)
	if proxy.creates.Load() != 0 || proxy.partAttempts.Load() != 0 {
		t.Fatalf("multipart calls after setup and connectivity = create:%d parts:%d, want 0/0", proxy.creates.Load(), proxy.partAttempts.Load())
	}
	const dumpBytes = 12 << 20
	output, exitCode := runCLIWithEnvironment(
		t,
		ctx,
		backupBinary,
		map[string]string{
			"AWS_MAX_ATTEMPTS":       "3",
			"AWS_RETRY_MODE":         "standard",
			"FAKE_DUMP_RANDOM_BYTES": strconv.Itoa(dumpBytes),
		},
		"backup", "production", "--config", configPath,
	)
	if exitCode != 0 {
		t.Fatalf("multipart backup exit code = %d, want 0\n%s", exitCode, output)
	}
	expectedObject, forwardedParts := proxy.forwardedObject(t)
	if proxy.creates.Load() != 1 || proxy.transientFailures.Load() != 1 || proxy.completes.Load() != 1 || proxy.aborts.Load() != 0 {
		t.Fatalf(
			"multipart proxy calls = create:%d transient-failures:%d complete:%d abort:%d, want 1/1/1/0",
			proxy.creates.Load(), proxy.transientFailures.Load(), proxy.completes.Load(), proxy.aborts.Load(),
		)
	}
	if got, want := proxy.partAttempts.Load(), int32(forwardedParts+1); got != want {
		t.Fatalf("UploadPart attempts = %d, want %d successful parts plus one retry", got, want)
	}

	objects, err := directClient.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	if err != nil {
		t.Fatalf("list integration objects: %v", err)
	}
	if len(objects.Contents) != 1 {
		t.Fatalf("object count = %d, want exactly 1", len(objects.Contents))
	}
	object, err := directClient.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    objects.Contents[0].Key,
	})
	if err != nil {
		t.Fatalf("fetch integration object: %v", err)
	}
	compressed, readErr := io.ReadAll(io.LimitReader(object.Body, (20<<20)+1))
	closeErr := object.Body.Close()
	if readErr != nil {
		t.Fatalf("read integration object: %v", readErr)
	}
	if closeErr != nil {
		t.Fatalf("close integration object: %v", closeErr)
	}
	if len(compressed) > 20<<20 {
		t.Fatalf("compressed multipart object exceeds test bound: %d bytes", len(compressed))
	}
	if len(compressed) <= 8<<20 {
		t.Fatalf("compressed object size = %d, want genuine multipart object above 8 MiB", len(compressed))
	}
	if !bytes.Equal(compressed, expectedObject) {
		t.Fatalf("completed object differs from the ordered bytes accepted by UploadPart: got %d bytes want %d", len(compressed), len(expectedObject))
	}
	gotChecksum := sha256.Sum256(compressed)
	wantChecksum := sha256.Sum256(expectedObject)
	if gotChecksum != wantChecksum {
		t.Fatalf("completed object SHA-256 = %x, want %x", gotChecksum, wantChecksum)
	}
	if got := len(gunzip(t, compressed)); got != dumpBytes {
		t.Fatalf("decompressed dump size = %d, want %d", got, dumpBytes)
	}
	uploads, err := directClient.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(bucket)})
	if err != nil {
		t.Fatalf("list multipart uploads after completion: %v", err)
	}
	if len(uploads.Uploads) != 0 {
		t.Fatalf("completed multipart uploads remain active: %#v", uploads.Uploads)
	}
	assertJSONLogs(t, logDirectory)
	assertNoStagedBackup(t, stageDirectory)
}

func TestBackupExhaustsMultipartRetriesThenAbortsAndCleansStaging(t *testing.T) {
	endpoint := os.Getenv("EZDBBACKUP_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("integration test requires EZDBBACKUP_TEST_S3_ENDPOINT (for example http://127.0.0.1:4566)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	repository := repositoryRoot(t)
	temporary := secureIntegrationTempDir(t)
	backupBinary := filepath.Join(temporary, "ezdbbackup")
	fakeDumpBinary := filepath.Join(temporary, "mysqldump-fake")
	buildBinary(t, ctx, repository, backupBinary, "./cmd/ezdbbackup")
	buildBinary(t, ctx, repository, fakeDumpBinary, "./test/fakedump")

	stageDirectory := filepath.Join(temporary, "stage")
	logDirectory := filepath.Join(temporary, "logs")
	for _, directory := range []string{stageDirectory, logDirectory} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatalf("create integration directory %q: %v", directory, err)
		}
	}
	currentUser, err := user.Current()
	if err != nil {
		t.Fatalf("look up current user: %v", err)
	}
	bucket := integrationBucket(temporary)
	directClient := newS3Client(t, ctx, endpoint)
	if _, err := directClient.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("create integration bucket: %v", err)
	}
	t.Cleanup(func() { cleanBucket(t, directClient, bucket) })

	proxy := newMultipartFailureProxy(t, endpoint)
	configPath := filepath.Join(temporary, "config.yml")
	writeConfig(t, configPath, integrationConfig{
		Bucket:         bucket,
		DumpBinary:     fakeDumpBinary,
		Endpoint:       proxy.server.URL,
		LogDirectory:   logDirectory,
		RunAs:          currentUser.Username,
		StageDirectory: stageDirectory,
	})
	output, exitCode := runCLIWithEnvironment(
		t,
		ctx,
		backupBinary,
		map[string]string{
			"AWS_MAX_ATTEMPTS":       "3",
			"AWS_RETRY_MODE":         "standard",
			"FAKE_DUMP_RANDOM_BYTES": strconv.Itoa(12 << 20),
		},
		"backup", "production", "--config", configPath,
	)
	if exitCode != 1 {
		t.Fatalf("multipart backup exit code = %d, want 1\n%s", exitCode, output)
	}
	if bytes.Contains(output, []byte("MULTIPART_ENDPOINT_SECRET")) {
		t.Fatalf("multipart failure output exposed endpoint body: %s", output)
	}
	assertLogsOmit(t, logDirectory, "MULTIPART_ENDPOINT_SECRET")
	if proxy.creates.Load() != 1 || proxy.failedParts.Load() != 3 || proxy.aborts.Load() != 1 {
		t.Fatalf("multipart proxy calls = create:%d failed-parts:%d abort:%d, want 1/3/1", proxy.creates.Load(), proxy.failedParts.Load(), proxy.aborts.Load())
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		uploads, err := directClient.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(bucket)})
		if err != nil {
			t.Fatalf("list multipart uploads: %v", err)
		}
		if len(uploads.Uploads) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("orphaned multipart uploads remain: %#v", uploads.Uploads)
		}
		time.Sleep(50 * time.Millisecond)
	}
	objects, err := directClient.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	if err != nil {
		t.Fatalf("list objects after multipart failure: %v", err)
	}
	if len(objects.Contents) != 0 {
		t.Fatalf("objects after multipart failure = %d, want 0", len(objects.Contents))
	}
	assertNoStagedBackup(t, stageDirectory)
}

type multipartFailureProxy struct {
	server      *httptest.Server
	creates     atomic.Int32
	failedParts atomic.Int32
	aborts      atomic.Int32
}

func newMultipartFailureProxy(t *testing.T, endpoint string) *multipartFailureProxy {
	t.Helper()
	target, err := url.Parse(endpoint)
	if err != nil {
		t.Fatalf("parse LocalStack endpoint: %v", err)
	}
	proxy := &multipartFailureProxy{}
	reverseProxy := httputil.NewSingleHostReverseProxy(target)
	proxy.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		switch {
		case request.Method == http.MethodPost && query.Has("uploads"):
			proxy.creates.Add(1)
		case request.Method == http.MethodPut && query.Get("partNumber") == "2" && query.Get("uploadId") != "":
			proxy.failedParts.Add(1)
			_, _ = io.Copy(io.Discard, request.Body)
			_ = request.Body.Close()
			writer.Header().Set("Content-Type", "application/xml")
			writer.Header().Set("Retry-After", "0")
			writer.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(writer, `<Error><Code>ServiceUnavailable</Code><Message>MULTIPART_ENDPOINT_SECRET `+request.URL.String()+`</Message></Error>`)
			return
		case request.Method == http.MethodDelete && query.Get("uploadId") != "":
			proxy.aborts.Add(1)
		}
		reverseProxy.ServeHTTP(writer, request)
	}))
	t.Cleanup(proxy.server.Close)
	return proxy
}

type transientMultipartProxy struct {
	server            *httptest.Server
	creates           atomic.Int32
	partAttempts      atomic.Int32
	transientFailures atomic.Int32
	completes         atomic.Int32
	aborts            atomic.Int32

	mu              sync.Mutex
	firstFailedPart []byte
	forwardedParts  map[int][]byte
	handlerErr      error
}

func newTransientMultipartUploadProxy(t *testing.T, endpoint string) *transientMultipartProxy {
	t.Helper()
	target, err := url.Parse(endpoint)
	if err != nil {
		t.Fatalf("parse LocalStack endpoint: %v", err)
	}
	proxy := &transientMultipartProxy{forwardedParts: make(map[int][]byte)}
	reverseProxy := httputil.NewSingleHostReverseProxy(target)
	proxy.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		switch {
		case request.Method == http.MethodPost && query.Has("uploads"):
			proxy.creates.Add(1)
		case request.Method == http.MethodPut && query.Get("partNumber") != "" && query.Get("uploadId") != "":
			partNumber, parseErr := strconv.Atoi(query.Get("partNumber"))
			if parseErr != nil || partNumber < 1 {
				proxy.recordHandlerError(fmt.Errorf("invalid UploadPart number %q", query.Get("partNumber")))
				http.Error(writer, "invalid multipart test request", http.StatusBadGateway)
				return
			}
			body, readErr := io.ReadAll(io.LimitReader(request.Body, (6<<20)+1))
			closeErr := request.Body.Close()
			if readErr != nil || closeErr != nil || len(body) > 6<<20 {
				proxy.recordHandlerError(fmt.Errorf("capture UploadPart %d: bytes=%d read=%v close=%v", partNumber, len(body), readErr, closeErr))
				http.Error(writer, "capture multipart test request", http.StatusBadGateway)
				return
			}
			proxy.partAttempts.Add(1)
			if proxy.transientFailures.CompareAndSwap(0, 1) {
				proxy.mu.Lock()
				proxy.firstFailedPart = bytes.Clone(body)
				proxy.mu.Unlock()
				writer.Header().Set("Content-Type", "application/xml")
				writer.Header().Set("Retry-After", "0")
				writer.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(writer, `<Error><Code>ServiceUnavailable</Code><Message>transient multipart upload fault</Message></Error>`)
				return
			}
			proxy.recordForwardedPart(partNumber, body)
			request.Body = io.NopCloser(bytes.NewReader(body))
			request.ContentLength = int64(len(body))
			request.GetBody = func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(body)), nil
			}
		case request.Method == http.MethodPost && query.Get("uploadId") != "":
			proxy.completes.Add(1)
		case request.Method == http.MethodDelete && query.Get("uploadId") != "":
			proxy.aborts.Add(1)
		}
		reverseProxy.ServeHTTP(writer, request)
	}))
	t.Cleanup(proxy.server.Close)
	return proxy
}

func (p *transientMultipartProxy) recordHandlerError(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.handlerErr == nil {
		p.handlerErr = err
	}
}

func (p *transientMultipartProxy) recordForwardedPart(partNumber int, body []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if partNumber == 1 && p.firstFailedPart != nil && !bytes.Equal(body, p.firstFailedPart) && p.handlerErr == nil {
		p.handlerErr = errors.New("retried first UploadPart body differs from the failed attempt")
	}
	if _, exists := p.forwardedParts[partNumber]; exists && p.handlerErr == nil {
		p.handlerErr = fmt.Errorf("UploadPart %d was forwarded more than once", partNumber)
	}
	p.forwardedParts[partNumber] = bytes.Clone(body)
}

func (p *transientMultipartProxy) forwardedObject(t *testing.T) ([]byte, int) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.handlerErr != nil {
		t.Fatalf("multipart fault proxy: %v", p.handlerErr)
	}
	if len(p.forwardedParts) == 0 {
		t.Fatal("multipart fault proxy forwarded no parts")
	}
	object := make([]byte, 0)
	for partNumber := 1; partNumber <= len(p.forwardedParts); partNumber++ {
		part, ok := p.forwardedParts[partNumber]
		if !ok {
			t.Fatalf("multipart fault proxy did not forward part %d", partNumber)
		}
		object = append(object, part...)
	}
	return object, len(p.forwardedParts)
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate integration test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
}

func secureIntegrationTempDir(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("secure integration temp directory: %v", err)
	}
	return directory
}

func buildBinary(t *testing.T, ctx context.Context, repository, output, target string) {
	t.Helper()
	command := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", output, target)
	command.Dir = repository
	command.Env = append(environmentWithout("CGO_ENABLED"), "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", target, err, output)
	}
	// go build honors the process umask and commonly emits 0775. Production
	// executable validation intentionally rejects group-writable programs.
	if err := os.Chmod(output, 0o755); err != nil {
		t.Fatalf("secure built test executable %s: %v", target, err)
	}
}

func newS3Client(t *testing.T, ctx context.Context, endpoint string) *s3.Client {
	t.Helper()
	configuration, err := awsconfig.LoadDefaultConfig(
		ctx,
		awsconfig.WithRegion(integrationRegion),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(testAccessKey, testSecretKey, "")),
	)
	if err != nil {
		t.Fatalf("load integration AWS configuration: %v", err)
	}
	return s3.NewFromConfig(configuration, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(endpoint)
		options.UsePathStyle = true
	})
}

func integrationBucket(temporary string) string {
	sum := sha256.Sum256([]byte(temporary))
	return "ezdbbackup-integration-" + hex.EncodeToString(sum[:6])
}

func cleanBucket(t *testing.T, client *s3.Client, bucket string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	uploads, err := client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(bucket)})
	if err != nil {
		t.Errorf("cleanup: list multipart uploads: %v", err)
		return
	}
	for _, upload := range uploads.Uploads {
		if _, err := client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
			Bucket: aws.String(bucket), Key: upload.Key, UploadId: upload.UploadId,
		}); err != nil {
			t.Errorf("cleanup: abort multipart upload: %v", err)
			return
		}
	}
	objects, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	if err != nil {
		t.Errorf("cleanup: list integration objects: %v", err)
		return
	}
	for _, object := range objects.Contents {
		if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: object.Key}); err != nil {
			t.Errorf("cleanup: delete integration object: %v", err)
			return
		}
	}
	if _, err := client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Errorf("cleanup: delete integration bucket: %v", err)
	}
}

type integrationConfig struct {
	Bucket         string
	DumpBinary     string
	Endpoint       string
	LogDirectory   string
	Password       string
	RunAs          string
	StageDirectory string
}

func writeConfig(t *testing.T, path string, values integrationConfig) {
	t.Helper()
	contents := fmt.Sprintf(`version: 1
defaults:
  dump_binary: %q
  temp_dir: %q
logging:
  directory: %q
  debug: false
  rotation:
    max_size_mb: 100
    max_files: 7
    max_age_days: 30
    compress: true
jobs:
  production:
    enabled: true
    schedule: "0 2 * * *"
    run_as: %q
    mysql:
      host: 127.0.0.1
      port: 3306
      user: backup
      password: %q
      databases: [application]
    s3:
      bucket: %q
      prefix: integration
      region: %s
      endpoint: %q
      force_path_style: true
      access_key_id: %q
      secret_access_key: %q
`, values.DumpBinary, values.StageDirectory, values.LogDirectory, values.RunAs, values.Password, values.Bucket, integrationRegion, values.Endpoint, testAccessKey, testSecretKey)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write integration config: %v", err)
	}
}

func runCLI(t *testing.T, ctx context.Context, binary string, arguments ...string) {
	t.Helper()
	command := exec.CommandContext(ctx, binary, arguments...)
	command.Env = environmentWithout("FAKE_DUMP_FAIL", "FAKE_DUMP_RANDOM_BYTES")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run ezdbbackup %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
}

func runCLIWithEnvironment(t *testing.T, ctx context.Context, binary string, environment map[string]string, arguments ...string) ([]byte, int) {
	t.Helper()
	names := make([]string, 0, len(environment))
	for name := range environment {
		names = append(names, name)
	}
	command := exec.CommandContext(ctx, binary, arguments...)
	command.Env = environmentWithout(names...)
	for name, value := range environment {
		command.Env = append(command.Env, name+"="+value)
	}
	output, err := command.CombinedOutput()
	if err == nil {
		return output, 0
	}
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		t.Fatalf("run ezdbbackup %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
	return output, exitError.ExitCode()
}

func gunzip(t *testing.T, compressed []byte) string {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("open gzip object: %v", err)
	}
	contents, err := io.ReadAll(reader)
	closeErr := reader.Close()
	if err != nil {
		t.Fatalf("read gzip object: %v", err)
	}
	if closeErr != nil {
		t.Fatalf("close gzip object: %v", closeErr)
	}
	return string(contents)
}

func environmentWithout(names ...string) []string {
	environment := os.Environ()
	filtered := make([]string, 0, len(environment))
	for _, entry := range environment {
		keep := true
		for _, name := range names {
			if strings.HasPrefix(entry, name+"=") {
				keep = false
				break
			}
		}
		if keep {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func assertJSONLogs(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read log directory: %v", err)
	}
	var stages []string
	lineCount := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		file, err := os.Open(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatalf("open %s: %v", entry.Name(), err)
		}
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			lineCount++
			var event struct {
				Job   string `json:"job"`
				Stage string `json:"stage"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
				_ = file.Close()
				t.Fatalf("parse %s JSON line %d: %v", entry.Name(), lineCount, err)
			}
			if event.Job != "production" {
				_ = file.Close()
				t.Fatalf("log job = %q, want production", event.Job)
			}
			if event.Stage == "" {
				_ = file.Close()
				t.Fatal("log stage is empty")
			}
			stages = append(stages, event.Stage)
		}
		if err := scanner.Err(); err != nil {
			_ = file.Close()
			t.Fatalf("scan %s: %v", entry.Name(), err)
		}
		if err := file.Close(); err != nil {
			t.Fatalf("close %s: %v", entry.Name(), err)
		}
	}
	if lineCount == 0 {
		t.Fatal("JSON logs contained no events")
	}
	wantStages := []string{"start", "temporary_storage", "s3_upload", "complete"}
	if !slices.Equal(stages, wantStages) {
		t.Fatalf("log stages = %v, want %v", stages, wantStages)
	}
}

func assertDumpFailureLog(t *testing.T, directory, password string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read log directory: %v", err)
	}
	found := false
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		contents, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		if bytes.Contains(contents, []byte(password)) {
			t.Fatalf("%s exposed MySQL password", entry.Name())
		}
		scanner := bufio.NewScanner(bytes.NewReader(contents))
		for scanner.Scan() {
			var event map[string]any
			if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
				t.Fatalf("parse %s JSON: %v", entry.Name(), err)
			}
			if event["level"] != "error" || event["stage"] != "dump_execution" {
				continue
			}
			found = true
			errorText, _ := event["error"].(string)
			if !strings.Contains(errorText, "[REDACTED]") {
				t.Fatalf("dump failure error = %q, want redaction marker", errorText)
			}
		}
		if err := scanner.Err(); err != nil {
			t.Fatalf("scan %s: %v", entry.Name(), err)
		}
	}
	if !found {
		t.Fatal("dump failure log event not found")
	}
}

func assertLogsOmit(t *testing.T, directory string, forbidden ...string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read log directory: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		contents, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		for _, value := range forbidden {
			if value != "" && bytes.Contains(contents, []byte(value)) {
				t.Fatalf("%s exposed forbidden text %q", entry.Name(), value)
			}
		}
	}
}

func assertNoStagedBackup(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("staging directory entries remain: %v", entries)
	}
}
