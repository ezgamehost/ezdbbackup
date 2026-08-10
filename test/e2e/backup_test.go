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
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
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
	temporary := t.TempDir()
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

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate integration test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
}

func buildBinary(t *testing.T, ctx context.Context, repository, output, target string) {
	t.Helper()
	command := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", output, target)
	command.Dir = repository
	command.Env = append(environmentWithout("CGO_ENABLED"), "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", target, err, output)
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
      databases: [application]
    s3:
      bucket: %q
      prefix: integration
      region: %s
      endpoint: %q
      force_path_style: true
      access_key_id: %q
      secret_access_key: %q
`, values.DumpBinary, values.StageDirectory, values.LogDirectory, values.RunAs, values.Bucket, integrationRegion, values.Endpoint, testAccessKey, testSecretKey)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write integration config: %v", err)
	}
}

func runCLI(t *testing.T, ctx context.Context, binary string, arguments ...string) {
	t.Helper()
	command := exec.CommandContext(ctx, binary, arguments...)
	command.Env = environmentWithout("FAKE_DUMP_FAIL")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run ezdbbackup %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
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

func assertNoStagedBackup(t *testing.T, directory string) {
	t.Helper()
	err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql.gz") {
			return fmt.Errorf("staged backup remains at %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
