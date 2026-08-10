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

func TestBackupRetriesTransientS3Upload(t *testing.T) {
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

	proxy, uploadAttempts := newTransientUploadProxy(t, endpoint, bucket)
	configPath := filepath.Join(temporary, "config.yml")
	writeConfig(t, configPath, integrationConfig{
		Bucket:         bucket,
		DumpBinary:     fakeDumpBinary,
		Endpoint:       proxy.URL,
		LogDirectory:   logDirectory,
		RunAs:          currentUser.Username,
		StageDirectory: stageDirectory,
	})

	runCLI(t, ctx, backupBinary, "validate", "--all", "--connectivity", "--config", configPath)
	if got := uploadAttempts.Load(); got != 0 {
		t.Fatalf("upload attempts after setup and connectivity = %d, want 0", got)
	}
	runCLI(t, ctx, backupBinary, "backup", "production", "--config", configPath)
	if got := uploadAttempts.Load(); got < 2 {
		t.Fatalf("upload attempts = %d, want at least 2 after one transient failure", got)
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
	compressed, readErr := io.ReadAll(io.LimitReader(object.Body, 1<<20))
	closeErr := object.Body.Close()
	if readErr != nil {
		t.Fatalf("read integration object: %v", readErr)
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

func TestBackupAbortsFailedMultipartUploadAndCleansStaging(t *testing.T) {
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
		map[string]string{"FAKE_DUMP_RANDOM_BYTES": strconv.Itoa(12 << 20)},
		"backup", "production", "--config", configPath,
	)
	if exitCode != 1 {
		t.Fatalf("multipart backup exit code = %d, want 1\n%s", exitCode, output)
	}
	if bytes.Contains(output, []byte("MULTIPART_ENDPOINT_SECRET")) {
		t.Fatalf("multipart failure output exposed endpoint body: %s", output)
	}
	assertLogsOmit(t, logDirectory, "MULTIPART_ENDPOINT_SECRET")
	if proxy.creates.Load() != 1 || proxy.failedParts.Load() == 0 || proxy.aborts.Load() != 1 {
		t.Fatalf("multipart proxy calls = create:%d failed-parts:%d abort:%d, want 1/>0/1", proxy.creates.Load(), proxy.failedParts.Load(), proxy.aborts.Load())
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
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(writer, `<Error><Code>InvalidRequest</Code><Message>MULTIPART_ENDPOINT_SECRET `+request.URL.String()+`</Message></Error>`)
			return
		case request.Method == http.MethodDelete && query.Get("uploadId") != "":
			proxy.aborts.Add(1)
		}
		reverseProxy.ServeHTTP(writer, request)
	}))
	t.Cleanup(proxy.server.Close)
	return proxy
}

func newTransientUploadProxy(t *testing.T, endpoint, bucket string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	target, err := url.Parse(endpoint)
	if err != nil {
		t.Fatalf("parse LocalStack endpoint: %v", err)
	}
	reverseProxy := httputil.NewSingleHostReverseProxy(target)
	uploadAttempts := &atomic.Int32{}
	uploadPathPrefix := "/" + bucket + "/integration/production/"
	proxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPut && strings.HasPrefix(request.URL.Path, uploadPathPrefix) {
			attempt := uploadAttempts.Add(1)
			if attempt == 1 {
				_, _ = io.Copy(io.Discard, request.Body)
				_ = request.Body.Close()
				writer.Header().Set("Content-Type", "application/xml")
				writer.Header().Set("Retry-After", "0")
				writer.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(writer, `<Error><Code>ServiceUnavailable</Code><Message>transient upload fault</Message></Error>`)
				return
			}
		}
		reverseProxy.ServeHTTP(writer, request)
	}))
	t.Cleanup(proxy.Close)
	return proxy, uploadAttempts
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
