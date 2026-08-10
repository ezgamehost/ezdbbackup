// Package backup orchestrates configured database backup jobs.
package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/ezgamehost/ezdbbackup/internal/config"
	"github.com/ezgamehost/ezdbbackup/internal/dump"
	"github.com/ezgamehost/ezdbbackup/internal/jobresolve"
	"github.com/ezgamehost/ezdbbackup/internal/logging"
	"github.com/ezgamehost/ezdbbackup/internal/stage"
	"github.com/ezgamehost/ezdbbackup/internal/storage"
)

// Service owns the staged lifecycle for one or more backup jobs.
type Service struct {
	Resolve  jobresolve.OptionsResolver
	Dump     dump.Runner
	Stager   stage.Stager
	Stores   storage.Factory
	Log      logging.Sink
	Now      func() time.Time
	Debug    bool
	Progress func(ProgressEvent)
}

// ProgressKind identifies a synchronous user-visible lifecycle transition.
type ProgressKind string

const (
	ProgressStarted       ProgressKind = "started"
	ProgressStaged        ProgressKind = "staged"
	ProgressUploadStarted ProgressKind = "upload_started"
	ProgressCompleted     ProgressKind = "completed"
	ProgressFailed        ProgressKind = "failed"
)

// ProgressEvent contains only curated, secret-safe lifecycle metadata.
type ProgressEvent struct {
	Kind      ProgressKind
	Job       string
	Stage     string
	ObjectKey string
	Size      int64
	Error     string
}

// Result describes a completed uploaded backup.
type Result struct {
	Job       string
	ObjectKey string
	Size      int64
	Duration  time.Duration
}

// StageError identifies the backup stage that failed while retaining its cause.
type StageError struct {
	Stage string
	Err   error
	text  string
}

func (e *StageError) Error() string {
	return fmt.Sprintf("backup stage %s: %s", e.Stage, e.text)
}

func (e *StageError) Unwrap() error { return e.Err }

// Run stages, uploads, and removes one job's compressed database dump.
func (s *Service) Run(ctx context.Context, jobName string, job config.JobConfig) (result Result, err error) {
	clock := s.now()
	started := clock()
	secrets := appendSecret(nil, job.MySQL.Password)
	secrets = appendSecret(secrets, job.S3.AccessKeyID)
	secrets = appendSecret(secrets, job.S3.SecretAccessKey)
	secrets = appendSecret(secrets, job.S3.SessionToken)
	logHealthy := true
	writeLog := func(event logging.Event) error {
		if !logHealthy {
			return nil
		}
		if writeErr := s.write(event); writeErr != nil {
			logHealthy = false
			return stageError("logging", writeErr, secrets...)
		}
		return nil
	}
	defer func() {
		finished := clock()
		duration := finished.Sub(started)
		if result.Job != "" {
			result.Duration = duration
		}
		if err != nil {
			if logHealthy {
				logErr := writeLog(logging.Event{
					Time:    finished,
					Level:   logging.ErrorLevel,
					Message: "backup failed",
					Command: "backup",
					Job:     jobName,
					Stage:   errorStageOf(err),
					Fields: map[string]any{
						"duration": duration,
						"error":    safeErrorText(err, secrets),
					},
				})
				if logErr != nil {
					err = errors.Join(err, logErr)
				}
			}
			s.progress(ProgressEvent{
				Kind:  ProgressFailed,
				Job:   jobName,
				Stage: errorStageOf(err),
				Error: safeErrorText(err, secrets),
			})
			return
		}
		if logErr := writeLog(logging.Event{
			Time:    finished,
			Level:   logging.InfoLevel,
			Message: "backup completed",
			Command: "backup",
			Job:     jobName,
			Stage:   "complete",
			Fields: map[string]any{
				"object_key": result.ObjectKey,
				"size":       result.Size,
				"duration":   result.Duration,
			},
		}); logErr != nil {
			err = logErr
			s.progress(ProgressEvent{
				Kind:  ProgressFailed,
				Job:   jobName,
				Stage: "logging",
				Error: safeErrorText(err, secrets),
			})
			return
		}
		s.progress(ProgressEvent{
			Kind:      ProgressCompleted,
			Job:       jobName,
			Stage:     "complete",
			ObjectKey: result.ObjectKey,
			Size:      result.Size,
		})
	}()

	if logErr := writeLog(logging.Event{
		Time:    started,
		Level:   logging.InfoLevel,
		Message: "backup started",
		Command: "backup",
		Job:     jobName,
		Stage:   "start",
	}); logErr != nil {
		return Result{}, logErr
	}
	s.progress(ProgressEvent{Kind: ProgressStarted, Job: jobName, Stage: "start"})

	dumpRequest, resolveErr := s.Resolve.Dump(job)
	if resolveErr != nil {
		return Result{}, stageError("configuration", resolveErr, secrets...)
	}
	secrets = appendSecret(secrets, dumpRequest.Password)
	if s.Debug {
		if logErr := writeLog(logging.Event{
			Time:    clock(),
			Level:   logging.DebugLevel,
			Message: "dump options resolved",
			Command: "backup",
			Job:     jobName,
			Stage:   "configuration",
			Fields: map[string]any{
				"all_databases":        dumpRequest.AllDatabases,
				"database_count":       len(dumpRequest.Databases),
				"extra_argument_count": len(dumpRequest.ExtraArgs),
			},
		}); logErr != nil {
			return Result{}, logErr
		}
	}

	artifact, stageErr := s.Stager.Stage(ctx, job.TempDir, func(writer io.Writer) error {
		return s.Dump.Run(ctx, dumpRequest, writer)
	})
	if stageErr != nil {
		return Result{}, stageError(failureStage(stageErr), stageErr, secrets...)
	}
	defer func() {
		if removeErr := s.Stager.Remove(artifact); removeErr != nil {
			cleanupErr := stageError("cleanup", removeErr, secrets...)
			if err == nil {
				err = cleanupErr
			} else {
				err = errors.Join(err, cleanupErr)
			}
			if logHealthy {
				logErr := writeLog(logging.Event{
					Time:    clock(),
					Level:   logging.ErrorLevel,
					Message: "temporary backup cleanup failed",
					Command: "backup",
					Job:     jobName,
					Stage:   "cleanup",
					Fields:  map[string]any{"error": safeErrorText(removeErr, secrets)},
				})
				if logErr != nil {
					err = errors.Join(err, logErr)
				}
			}
		}
	}()
	if logErr := writeLog(logging.Event{
		Time:    clock(),
		Level:   logging.InfoLevel,
		Message: "backup staged",
		Command: "backup",
		Job:     jobName,
		Stage:   "temporary_storage",
		Fields:  map[string]any{"size": artifact.Size},
	}); logErr != nil {
		return Result{}, logErr
	}
	s.progress(ProgressEvent{Kind: ProgressStaged, Job: jobName, Stage: "temporary_storage", Size: artifact.Size})

	storageOptions, resolveErr := s.Resolve.Storage(job)
	if resolveErr != nil {
		return Result{}, stageError("configuration", resolveErr, secrets...)
	}
	secrets = appendSecret(secrets, storageOptions.Credentials.AccessKeyID)
	secrets = appendSecret(secrets, storageOptions.Credentials.SecretAccessKey)
	secrets = appendSecret(secrets, storageOptions.Credentials.SessionToken)
	if s.Debug {
		if logErr := writeLog(logging.Event{
			Time:    clock(),
			Level:   logging.DebugLevel,
			Message: "storage options resolved",
			Command: "backup",
			Job:     jobName,
			Stage:   "configuration",
			Fields: map[string]any{
				"custom_endpoint":      storageOptions.Endpoint != "",
				"explicit_credentials": storageOptions.Credentials.Explicit,
				"force_path_style":     storageOptions.ForcePathStyle,
			},
		}); logErr != nil {
			return Result{}, logErr
		}
	}

	store, storeErr := s.Stores.New(ctx, storageOptions)
	if storeErr != nil {
		return Result{}, stageError("s3_upload", storeErr, secrets...)
	}
	file, openErr := s.Stager.Open(artifact)
	if openErr != nil {
		return Result{}, stageError("temporary_storage", openErr, secrets...)
	}
	objectKey := storage.ObjectKey(job.S3.Prefix, jobName, started)
	if logErr := writeLog(logging.Event{
		Time:    clock(),
		Level:   logging.InfoLevel,
		Message: "backup upload started",
		Command: "backup",
		Job:     jobName,
		Stage:   "s3_upload",
		Fields:  map[string]any{"object_key": objectKey, "size": artifact.Size},
	}); logErr != nil {
		if closeErr := file.Close(); closeErr != nil {
			logErr = errors.Join(logErr, stageError("temporary_storage", closeErr, secrets...))
		}
		return Result{}, logErr
	}
	s.progress(ProgressEvent{
		Kind:      ProgressUploadStarted,
		Job:       jobName,
		Stage:     "s3_upload",
		ObjectKey: objectKey,
		Size:      artifact.Size,
	})
	_, uploadErr := store.UploadFile(ctx, job.S3.Bucket, objectKey, file, artifact.Size)
	closeErr := file.Close()
	if uploadErr != nil {
		uploadStageErr := stageError("s3_upload", uploadErr, secrets...)
		if closeErr != nil {
			uploadStageErr = errors.Join(uploadStageErr, stageError("temporary_storage", closeErr, secrets...))
		}
		return Result{}, uploadStageErr
	}
	if closeErr != nil {
		return Result{}, stageError("temporary_storage", closeErr, secrets...)
	}

	result = Result{Job: jobName, ObjectKey: objectKey, Size: artifact.Size}
	return result, nil
}

func stageError(stageName string, err error, secrets ...string) error {
	return &StageError{Stage: stageName, Err: err, text: safeErrorText(err, secrets)}
}

func errorStageOf(err error) string {
	var stageErr *StageError
	if errors.As(err, &stageErr) {
		return stageErr.Stage
	}
	return "unknown"
}

func failureStage(err error) string {
	switch failure := err.(type) {
	case *dump.RunError:
		var localFailure *stage.Error
		if errors.As(failure.Err, &localFailure) {
			return stageFailureName(localFailure)
		}
		if failure.Kind == dump.FailureStartup {
			return "dump_startup"
		}
		return "dump_execution"
	case *stage.Error:
		return stageFailureName(failure)
	}
	if aggregate, ok := err.(interface{ Unwrap() []error }); ok {
		branches := aggregate.Unwrap()
		if len(branches) > 0 {
			return failureStage(branches[0])
		}
	}
	if wrapper, ok := err.(interface{ Unwrap() error }); ok {
		return failureStage(wrapper.Unwrap())
	}
	return "dump_execution"
}

func stageFailureName(failure *stage.Error) string {
	if failure == nil {
		return "dump_execution"
	}
	switch failure.Kind {
	case stage.FailureCompression:
		return "compression"
	case stage.FailureTemporaryStorage:
		return "temporary_storage"
	default:
		return "dump_execution"
	}
}

func (s *Service) now() func() time.Time {
	if s.Now != nil {
		return s.Now
	}
	return time.Now
}

func (s *Service) write(event logging.Event) error {
	if s.Log != nil {
		return s.Log.Write(event)
	}
	return nil
}

func (s *Service) progress(event ProgressEvent) {
	if s.Progress != nil {
		s.Progress(event)
	}
}

func appendSecret(secrets []string, value string) []string {
	if value == "" {
		return secrets
	}
	return append(secrets, value)
}

func safeErrorText(err error, secrets []string) string {
	text := err.Error()
	ordered := append([]string(nil), secrets...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return len(ordered[i]) > len(ordered[j])
	})
	for _, secret := range ordered {
		text = strings.ReplaceAll(text, secret, "[REDACTED]")
	}
	return text
}
