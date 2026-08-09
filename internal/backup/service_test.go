package backup

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ezgamehost/ezdbbackup/internal/config"
	"github.com/ezgamehost/ezdbbackup/internal/dump"
	"github.com/ezgamehost/ezdbbackup/internal/jobresolve"
	"github.com/ezgamehost/ezdbbackup/internal/logging"
	"github.com/ezgamehost/ezdbbackup/internal/stage"
	"github.com/ezgamehost/ezdbbackup/internal/storage"
)

type dumpFunc func(context.Context, dump.Request, io.Writer) error

func (f dumpFunc) Run(ctx context.Context, request dump.Request, writer io.Writer) error {
	return f(ctx, request, writer)
}

func (dumpFunc) Probe(context.Context, dump.Request) error { return nil }

type memorySink struct {
	events []logging.Event
}

func (s *memorySink) Write(event logging.Event) error {
	s.events = append(s.events, event)
	return nil
}

type recordingFactory struct {
	options []storage.Options
	store   storage.Store
	err     error
}

func (f *recordingFactory) New(_ context.Context, options storage.Options) (storage.Store, error) {
	f.options = append(f.options, options)
	return f.store, f.err
}

type uploadStore struct {
	calls      int
	bucket     string
	key        string
	path       string
	contents   string
	exists     bool
	uploadErr  error
	uploadDone bool
}

func (s *uploadStore) UploadFile(_ context.Context, bucket, key, path string) (storage.UploadResult, error) {
	s.calls++
	s.bucket, s.key, s.path = bucket, key, path
	_, statErr := os.Stat(path)
	s.exists = statErr == nil
	file, err := os.Open(path)
	if err != nil {
		return storage.UploadResult{}, err
	}
	reader, err := gzip.NewReader(file)
	if err != nil {
		_ = file.Close()
		return storage.UploadResult{}, err
	}
	data, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	fileCloseErr := file.Close()
	if readErr != nil {
		return storage.UploadResult{}, readErr
	}
	if closeErr != nil {
		return storage.UploadResult{}, closeErr
	}
	if fileCloseErr != nil {
		return storage.UploadResult{}, fileCloseErr
	}
	s.contents = string(data)
	s.uploadDone = true
	return storage.UploadResult{}, s.uploadErr
}

func (*uploadStore) Probe(context.Context, string) error { return nil }

type removeErrorStager struct {
	stage.GzipStager
	err error
}

func (s removeErrorStager) Remove(artifact stage.Artifact) error {
	removeErr := s.GzipStager.Remove(artifact)
	if removeErr != nil {
		return errors.Join(s.err, removeErr)
	}
	return s.err
}

func TestRunStagesAndUploadsDumpThenRemovesArtifact(t *testing.T) {
	start := time.Date(2026, time.August, 9, 12, 34, 56, 789, time.FixedZone("test", -7*60*60))
	finish := start.Add(1500 * time.Millisecond)
	nowCalls := 0
	now := func() time.Time {
		nowCalls++
		if nowCalls == 1 {
			return start
		}
		return finish
	}
	store := &uploadStore{}
	factory := &recordingFactory{store: store}
	logs := &memorySink{}
	service := Service{
		Resolve: jobresolve.Resolver{},
		Dump: dumpFunc(func(_ context.Context, request dump.Request, writer io.Writer) error {
			want := dump.Request{
				Binary:       "/usr/local/bin/mysqldump",
				Host:         "db.internal",
				Port:         3307,
				User:         "backup",
				Password:     "mysql-secret",
				AllDatabases: true,
				ExtraArgs:    []string{"--quick"},
			}
			if !reflect.DeepEqual(request, want) {
				t.Fatalf("dump request = %#v, want %#v", request, want)
			}
			_, err := io.WriteString(writer, "CREATE TABLE widgets(id INT);\n")
			return err
		}),
		Stager: stage.GzipStager{},
		Stores: factory,
		Log:    logs,
		Now:    now,
	}
	job := config.JobConfig{
		Enabled:    true,
		DumpBinary: "/usr/local/bin/mysqldump",
		TempDir:    t.TempDir(),
		MySQL: config.MySQLConfig{
			Host:      "db.internal",
			Port:      3307,
			User:      "backup",
			Password:  "mysql-secret",
			Databases: config.DatabaseSelection{All: true},
			ExtraArgs: []string{"--quick"},
		},
		S3: config.S3Config{
			Bucket:          "database-backups",
			Prefix:          "daily/mysql",
			Region:          "us-west-2",
			Endpoint:        "https://objects.internal",
			ForcePathStyle:  true,
			AccessKeyID:     "s3-access-secret",
			SecretAccessKey: "s3-key-secret",
			SessionToken:    "s3-session-secret",
		},
	}

	result, err := service.Run(context.Background(), "production", job)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	wantKey := "daily/mysql/production/2026/08/09/production-20260809T193456.000000789Z.sql.gz"
	if result.Job != "production" || result.ObjectKey != wantKey || result.Size <= 0 || result.Duration != 1500*time.Millisecond {
		t.Fatalf("Run() result = %#v, want job/key/non-zero size/1.5s", result)
	}
	if len(factory.options) != 1 {
		t.Fatalf("storage factory calls = %d, want 1", len(factory.options))
	}
	wantStorage := storage.Options{
		Region:         "us-west-2",
		Endpoint:       "https://objects.internal",
		ForcePathStyle: true,
		Credentials: storage.Credentials{
			AccessKeyID:     "s3-access-secret",
			SecretAccessKey: "s3-key-secret",
			SessionToken:    "s3-session-secret",
			Explicit:        true,
		},
	}
	if !reflect.DeepEqual(factory.options[0], wantStorage) {
		t.Fatalf("storage options = %#v, want %#v", factory.options[0], wantStorage)
	}
	if store.calls != 1 || store.bucket != "database-backups" || store.key != wantKey {
		t.Fatalf("upload = calls:%d bucket:%q key:%q", store.calls, store.bucket, store.key)
	}
	if !store.exists || !store.uploadDone || store.contents != "CREATE TABLE widgets(id INT);\n" {
		t.Fatalf("staged upload = exists:%t complete:%t contents:%q", store.exists, store.uploadDone, store.contents)
	}
	if _, err := os.Stat(store.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged artifact after Run() = %v, want removed", err)
	}
	assertCompletionEvent(t, logs.events, result)
	encodedEvents := fmt.Sprint(logs.events)
	for _, secret := range []string{"mysql-secret", "s3-access-secret", "s3-key-secret", "s3-session-secret"} {
		if strings.Contains(encodedEvents, secret) {
			t.Fatalf("logs contain secret %q: %s", secret, encodedEvents)
		}
	}
}

func TestRunDumpFailurePreventsStoreCreationAndUpload(t *testing.T) {
	dumpErr := errors.New("mysqldump exited 2")
	store := &uploadStore{}
	factory := &recordingFactory{store: store}
	service := Service{
		Resolve: jobresolve.Resolver{},
		Dump:    dumpFunc(func(context.Context, dump.Request, io.Writer) error { return dumpErr }),
		Stager:  stage.GzipStager{},
		Stores:  factory,
		Log:     &memorySink{},
	}
	job := config.JobConfig{TempDir: t.TempDir(), MySQL: config.MySQLConfig{Databases: config.DatabaseSelection{All: true}}}

	_, err := service.Run(context.Background(), "production", job)
	if !errors.Is(err, dumpErr) {
		t.Fatalf("Run() error = %v, want dump error", err)
	}
	if got := errorStage(err); got != "dump_execution" {
		t.Fatalf("Run() error stage = %q, want dump_execution", got)
	}
	if len(factory.options) != 0 || store.calls != 0 {
		t.Fatalf("storage after dump failure = factory:%d upload:%d, want none", len(factory.options), store.calls)
	}
}

func TestRunUploadFailureStillRemovesArtifact(t *testing.T) {
	uploadErr := errors.New("upload unavailable")
	store := &uploadStore{uploadErr: uploadErr}
	service := successfulService(t, store, stage.GzipStager{}, &memorySink{})

	_, err := service.Run(context.Background(), "production", backupJob(t))
	if !errors.Is(err, uploadErr) {
		t.Fatalf("Run() error = %v, want upload error", err)
	}
	if got := errorStage(err); got != "s3_upload" {
		t.Fatalf("Run() error stage = %q, want s3_upload", got)
	}
	if _, statErr := os.Stat(store.path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("artifact after upload failure = %v, want removed", statErr)
	}
}

func TestRunReturnsCleanupFailureAfterOtherwiseSuccessfulBackup(t *testing.T) {
	cleanupErr := errors.New("cleanup denied")
	store := &uploadStore{}
	service := successfulService(t, store, removeErrorStager{err: cleanupErr}, &memorySink{})

	result, err := service.Run(context.Background(), "production", backupJob(t))
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("Run() error = %v, want cleanup error", err)
	}
	if got := errorStage(err); got != "cleanup" {
		t.Fatalf("Run() error stage = %q, want cleanup", got)
	}
	if result.Job != "production" || result.ObjectKey == "" {
		t.Fatalf("Run() result = %#v, want completed upload metadata", result)
	}
}

func TestRunCleanupFailureDoesNotMaskUploadFailure(t *testing.T) {
	uploadErr := errors.New("upload unavailable")
	cleanupErr := errors.New("cleanup denied")
	store := &uploadStore{uploadErr: uploadErr}
	logs := &memorySink{}
	service := successfulService(t, store, removeErrorStager{err: cleanupErr}, logs)

	_, err := service.Run(context.Background(), "production", backupJob(t))
	if !errors.Is(err, uploadErr) {
		t.Fatalf("Run() error = %v, want upload error", err)
	}
	if errors.Is(err, cleanupErr) {
		t.Fatalf("Run() error = %v, must not include later cleanup error", err)
	}
	foundCleanupLog := false
	for _, event := range logs.events {
		if event.Job == "production" && event.Stage == "cleanup" && event.Level == logging.ErrorLevel {
			foundCleanupLog = true
		}
	}
	if !foundCleanupLog {
		t.Fatalf("cleanup failure log not found: %#v", logs.events)
	}
}

func TestRunRedactsOverlappingSecretsFromFailureLogs(t *testing.T) {
	uploadErr := errors.New("upload rejected overlap and overlap-credential")
	store := &uploadStore{uploadErr: uploadErr}
	logs := &memorySink{}
	service := successfulService(t, store, stage.GzipStager{}, logs)
	job := backupJob(t)
	job.S3.AccessKeyID = "overlap"
	job.S3.SecretAccessKey = "overlap-credential"

	_, _ = service.Run(context.Background(), "production", job)
	for _, event := range logs.events {
		if event.Message != "backup failed" {
			continue
		}
		if got := event.Fields["error"]; got != "backup stage s3_upload: upload rejected [REDACTED] and [REDACTED]" {
			t.Fatalf("failure log error = %q, want complete redaction", got)
		}
		return
	}
	t.Fatalf("backup failure event not found: %#v", logs.events)
}

func successfulService(t *testing.T, store storage.Store, stager stage.Stager, logs logging.Sink) Service {
	t.Helper()
	return Service{
		Resolve: jobresolve.Resolver{},
		Dump: dumpFunc(func(_ context.Context, _ dump.Request, writer io.Writer) error {
			_, err := io.WriteString(writer, "SELECT 1;\n")
			return err
		}),
		Stager: stager,
		Stores: &recordingFactory{store: store},
		Log:    logs,
	}
}

func backupJob(t *testing.T) config.JobConfig {
	t.Helper()
	return config.JobConfig{
		Enabled: true,
		TempDir: t.TempDir(),
		MySQL:   config.MySQLConfig{Databases: config.DatabaseSelection{All: true}},
		S3:      config.S3Config{Bucket: "backups", Region: "us-east-1"},
	}
}

func errorStage(err error) string {
	var stageErr *StageError
	if errors.As(err, &stageErr) {
		return stageErr.Stage
	}
	return ""
}

func assertCompletionEvent(t *testing.T, events []logging.Event, result Result) {
	t.Helper()
	for _, event := range events {
		if event.Message != "backup completed" {
			continue
		}
		if event.Job != result.Job || event.Stage != "complete" {
			t.Fatalf("completion event identity = %#v", event)
		}
		if event.Fields["object_key"] != result.ObjectKey || event.Fields["size"] != result.Size || event.Fields["duration"] != result.Duration {
			t.Fatalf("completion event fields = %#v, want key/size/duration from %#v", event.Fields, result)
		}
		return
	}
	t.Fatalf("completion event not found: %#v", events)
}
