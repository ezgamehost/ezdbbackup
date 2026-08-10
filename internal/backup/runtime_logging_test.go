package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ezgamehost/ezdbbackup/internal/dump"
	"github.com/ezgamehost/ezdbbackup/internal/logging"
	"github.com/ezgamehost/ezdbbackup/internal/stage"
	"github.com/ezgamehost/ezdbbackup/internal/storage"
)

type messageFailingSink struct {
	message string
	err     error
	events  []logging.Event
}

func (s *messageFailingSink) Write(event logging.Event) error {
	s.events = append(s.events, event)
	if event.Message == s.message {
		return s.err
	}
	return nil
}

func TestRunStartLogFailureStopsBeforeDump(t *testing.T) {
	logErr := errors.New("start log unavailable")
	sink := &messageFailingSink{message: "backup started", err: logErr}
	dumpCalls := 0
	store := &uploadStore{}
	service := successfulService(t, store, stage.GzipStager{}, sink)
	service.Dump = dumpFunc(func(context.Context, dump.Request, io.Writer) error {
		dumpCalls++
		return nil
	})

	_, err := service.Run(context.Background(), "production", backupJob(t))
	if !errors.Is(err, logErr) || errorStage(err) != "logging" {
		t.Fatalf("Run() error = %v, want logging stage with original cause", err)
	}
	if dumpCalls != 0 || store.calls != 0 {
		t.Fatalf("effects after start-log failure = dump:%d upload:%d", dumpCalls, store.calls)
	}
	if got := eventMessages(sink.events); strings.Join(got, ",") != "backup started" {
		t.Fatalf("log calls = %v, want no write after first sink failure", got)
	}
}

func TestRunStagedLogFailureStopsBeforeStorageSideEffects(t *testing.T) {
	logErr := errors.New("staged log unavailable")
	sink := &messageFailingSink{message: "backup staged", err: logErr}
	store := &uploadStore{}
	factory := &recordingFactory{store: store}
	service := successfulService(t, store, stage.GzipStager{}, sink)
	service.Stores = factory

	_, err := service.Run(context.Background(), "production", backupJob(t))
	if !errors.Is(err, logErr) || errorStage(err) != "logging" {
		t.Fatalf("Run() error = %v, want logging stage", err)
	}
	if len(factory.options) != 0 || store.calls != 0 {
		t.Fatalf("storage effects after staged-log failure = factory:%d upload:%d", len(factory.options), store.calls)
	}
	if len(sink.events) == 0 || sink.events[len(sink.events)-1].Message != "backup staged" {
		t.Fatalf("events = %#v, want staged failure as final sink call", sink.events)
	}
}

func TestRunUploadStartLogFailureStopsBeforeUpload(t *testing.T) {
	logErr := errors.New("upload transition log unavailable")
	sink := &messageFailingSink{message: "backup upload started", err: logErr}
	store := &uploadStore{}
	service := successfulService(t, store, stage.GzipStager{}, sink)

	_, err := service.Run(context.Background(), "production", backupJob(t))
	if !errors.Is(err, logErr) || errorStage(err) != "logging" {
		t.Fatalf("Run() error = %v, want logging stage", err)
	}
	if store.calls != 0 {
		t.Fatalf("upload calls = %d, want zero after upload-start log failure", store.calls)
	}
	if len(sink.events) == 0 || sink.events[len(sink.events)-1].Message != "backup upload started" {
		t.Fatalf("events = %#v, want upload-start failure as final sink call", sink.events)
	}
}

func TestRunCompletionLogFailureReturnsFailureAfterUpload(t *testing.T) {
	logErr := errors.New("completion log unavailable")
	sink := &messageFailingSink{message: "backup completed", err: logErr}
	store := &uploadStore{}
	service := successfulService(t, store, stage.GzipStager{}, sink)

	result, err := service.Run(context.Background(), "production", backupJob(t))
	if !errors.Is(err, logErr) || errorStage(err) != "logging" {
		t.Fatalf("Run() error = %v, want logging stage", err)
	}
	if store.calls != 1 || result.ObjectKey == "" {
		t.Fatalf("completed upload = calls:%d result:%#v, want retained object metadata", store.calls, result)
	}
}

func TestRunFailureLogErrorJoinsBehindPrimaryStage(t *testing.T) {
	dumpCause := errors.New("dump primary")
	logCause := errors.New("failure log unavailable")
	sink := &messageFailingSink{message: "backup failed", err: logCause}
	service := successfulService(t, &uploadStore{}, stage.GzipStager{}, sink)
	service.Dump = dumpFunc(func(context.Context, dump.Request, io.Writer) error {
		return &dump.RunError{Kind: dump.FailureExecution, Err: dumpCause}
	})

	_, err := service.Run(context.Background(), "production", backupJob(t))
	if !errors.Is(err, dumpCause) || !errors.Is(err, logCause) {
		t.Fatalf("Run() error = %v, want primary and logging causes", err)
	}
	if got := errorStage(err); got != "dump_execution" {
		t.Fatalf("Run() stage = %q, want original dump_execution", got)
	}
	if len(sink.events) == 0 || sink.events[len(sink.events)-1].Message != "backup failed" {
		t.Fatalf("events = %#v, want failure log as final sink call", sink.events)
	}
}

func TestRunCleanupLogErrorJoinsBehindUploadAndCleanup(t *testing.T) {
	uploadCause := errors.New("upload primary")
	cleanupCause := errors.New("cleanup secondary")
	logCause := errors.New("cleanup log unavailable")
	sink := &messageFailingSink{message: "temporary backup cleanup failed", err: logCause}
	service := successfulService(t, &uploadStore{uploadErr: uploadCause}, removeErrorStager{err: cleanupCause}, sink)

	_, err := service.Run(context.Background(), "production", backupJob(t))
	for _, cause := range []error{uploadCause, cleanupCause, logCause} {
		if !errors.Is(err, cause) {
			t.Fatalf("Run() error = %v, want joined cause %v", err, cause)
		}
	}
	if got := errorStage(err); got != "s3_upload" {
		t.Fatalf("Run() stage = %q, want primary s3_upload", got)
	}
	if len(sink.events) == 0 || sink.events[len(sink.events)-1].Message != "temporary backup cleanup failed" {
		t.Fatalf("events = %#v, want cleanup log as final sink call", sink.events)
	}
}

func TestRunRedactsSecretsFromLoggingFailure(t *testing.T) {
	for _, message := range []string{"backup started", "backup staged"} {
		t.Run(message, func(t *testing.T) {
			const secret = "logging-secret-value"
			logCause := errors.New("cannot log " + secret)
			sink := &messageFailingSink{message: message, err: logCause}
			service := successfulService(t, &uploadStore{}, stage.GzipStager{}, sink)
			job := backupJob(t)
			job.MySQL.Password = secret

			_, err := service.Run(context.Background(), "production", job)
			if !errors.Is(err, logCause) {
				t.Fatalf("Run() error = %v, want original typed logging cause", err)
			}
			if strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[REDACTED]") {
				t.Fatalf("Run() error = %q, want secret-safe text", err)
			}
		})
	}
}

func TestFailureStagePrefersTypedLocalDestinationInsideDumpError(t *testing.T) {
	cause := errors.New("no space left on device")
	local := &stage.Error{Kind: stage.FailureTemporaryStorage, Err: cause}
	err := &dump.RunError{Kind: dump.FailureExecution, Err: local}
	if got := failureStage(err); got != "temporary_storage" {
		t.Fatalf("failureStage() = %q, want temporary_storage", got)
	}
	if !errors.Is(err, cause) {
		t.Fatal("typed destination cause was not preserved")
	}
}

func TestRunEmitsSequentialProgressEvents(t *testing.T) {
	service := successfulService(t, &uploadStore{}, stage.GzipStager{}, &memorySink{})
	var progress []ProgressEvent
	service.Progress = func(event ProgressEvent) {
		progress = append(progress, event)
	}

	result, err := service.Run(context.Background(), "production", backupJob(t))
	if err != nil {
		t.Fatal(err)
	}
	want := []ProgressKind{ProgressStarted, ProgressStaged, ProgressUploadStarted, ProgressCompleted}
	if len(progress) != len(want) {
		t.Fatalf("progress = %#v, want %v", progress, want)
	}
	for i, kind := range want {
		if progress[i].Kind != kind || progress[i].Job != "production" {
			t.Fatalf("progress[%d] = %#v, want kind %q/job production", i, progress[i], kind)
		}
	}
	if progress[1].Size <= 0 || progress[2].ObjectKey == "" || progress[3].ObjectKey != result.ObjectKey {
		t.Fatalf("progress metadata = %#v, want staged size and final object key", progress)
	}
}

func TestRunProgressAppearsBeforeDumpAndUploadUnblock(t *testing.T) {
	startSeen := make(chan struct{})
	releaseStart := make(chan struct{})
	uploadSeen := make(chan struct{})
	releaseUpload := make(chan struct{})
	var dumpCalls atomic.Int32
	store := &uploadStore{}
	service := successfulService(t, store, stage.GzipStager{}, &memorySink{})
	service.Dump = dumpFunc(func(_ context.Context, _ dump.Request, writer io.Writer) error {
		dumpCalls.Add(1)
		_, err := io.WriteString(writer, "SELECT 1;\n")
		return err
	})
	service.Progress = func(event ProgressEvent) {
		switch event.Kind {
		case ProgressStarted:
			close(startSeen)
			<-releaseStart
		case ProgressUploadStarted:
			close(uploadSeen)
			<-releaseUpload
		}
	}
	done := make(chan error, 1)
	go func() {
		_, err := service.Run(context.Background(), "production", backupJob(t))
		done <- err
	}()

	<-startSeen
	if dumpCalls.Load() != 0 {
		t.Fatalf("dump calls = %d before start progress unblocked", dumpCalls.Load())
	}
	close(releaseStart)
	<-uploadSeen
	if store.calls != 0 {
		t.Fatalf("upload calls = %d before upload-start progress unblocked", store.calls)
	}
	close(releaseUpload)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRunEmitsOneSecretSafeFailureProgressEvent(t *testing.T) {
	const secret = "progress-secret"
	service := successfulService(t, &uploadStore{uploadErr: errors.New("rejected " + secret)}, stage.GzipStager{}, &memorySink{})
	job := backupJob(t)
	job.S3.SecretAccessKey = secret
	job.S3.AccessKeyID = "access"
	var progress []ProgressEvent
	service.Progress = func(event ProgressEvent) { progress = append(progress, event) }

	_, err := service.Run(context.Background(), "production", job)
	if err == nil {
		t.Fatal("Run() error = nil")
	}
	failures := 0
	for _, event := range progress {
		if event.Kind != ProgressFailed {
			continue
		}
		failures++
		if strings.Contains(event.Error, secret) || !strings.Contains(event.Error, "[REDACTED]") {
			t.Fatalf("failure progress = %#v, want redacted error", event)
		}
	}
	if failures != 1 {
		t.Fatalf("progress = %#v, want exactly one failure", progress)
	}
}

func TestRunDebugEventsAreMeaningfulAndSecretFree(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("enabled=%t", enabled), func(t *testing.T) {
			logs := &memorySink{}
			service := successfulService(t, &uploadStore{}, stage.GzipStager{}, logs)
			service.Debug = enabled
			job := backupJob(t)
			job.MySQL.Password = "mysql-debug-secret"
			job.S3.AccessKeyID = "access-debug-secret"
			job.S3.SecretAccessKey = "key-debug-secret"

			if _, err := service.Run(context.Background(), "production", job); err != nil {
				t.Fatal(err)
			}
			debugCount := 0
			for _, event := range logs.events {
				if event.Level == logging.DebugLevel {
					debugCount++
					if event.Stage == "" || len(event.Fields) == 0 {
						t.Fatalf("debug event lacks resolved stage metadata: %#v", event)
					}
				}
			}
			if enabled && debugCount < 2 {
				t.Fatalf("debug events = %#v, want resolved dump and storage diagnostics", logs.events)
			}
			if !enabled && debugCount != 0 {
				t.Fatalf("debug events emitted while disabled: %#v", logs.events)
			}
			encoded := fmt.Sprint(logs.events)
			for _, secret := range []string{"mysql-debug-secret", "access-debug-secret", "key-debug-secret"} {
				if strings.Contains(encoded, secret) {
					t.Fatalf("debug logs contain secret %q: %s", secret, encoded)
				}
			}
		})
	}
}

func eventMessages(events []logging.Event) []string {
	messages := make([]string, len(events))
	for i, event := range events {
		messages[i] = event.Message
	}
	return messages
}

var _ storage.Store = (*uploadStore)(nil)
