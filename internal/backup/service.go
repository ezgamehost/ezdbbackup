// Package backup orchestrates configured database backup jobs.
package backup

import (
	"context"
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
	Resolve jobresolve.OptionsResolver
	Dump    dump.Runner
	Stager  stage.Stager
	Stores  storage.Factory
	Log     logging.Sink
	Now     func() time.Time
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
}

func (e *StageError) Error() string {
	return fmt.Sprintf("backup stage %s: %v", e.Stage, e.Err)
}

func (e *StageError) Unwrap() error { return e.Err }

// Run stages, uploads, and removes one job's compressed database dump.
func (s *Service) Run(ctx context.Context, jobName string, job config.JobConfig) (result Result, err error) {
	started := s.now()()
	var secrets []string
	defer func() {
		finished := s.now()()
		duration := finished.Sub(started)
		if result.Job != "" {
			result.Duration = duration
		}
		if err != nil {
			s.write(logging.Event{
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
			return
		}
		s.write(logging.Event{
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
		})
	}()

	s.write(logging.Event{
		Time:    started,
		Level:   logging.InfoLevel,
		Message: "backup started",
		Command: "backup",
		Job:     jobName,
		Stage:   "start",
	})

	dumpRequest, resolveErr := s.Resolve.Dump(job)
	if resolveErr != nil {
		return Result{}, stageError("configuration", resolveErr)
	}
	secrets = appendSecret(secrets, dumpRequest.Password)

	artifact, stageErr := s.Stager.Stage(ctx, job.TempDir, func(writer io.Writer) error {
		return s.Dump.Run(ctx, dumpRequest, writer)
	})
	if stageErr != nil {
		return Result{}, stageError("dump_execution", stageErr)
	}
	defer func() {
		if removeErr := s.Stager.Remove(artifact); removeErr != nil {
			if err == nil {
				err = stageError("cleanup", removeErr)
			} else {
				s.write(logging.Event{
					Time:    started,
					Level:   logging.ErrorLevel,
					Message: "temporary backup cleanup failed",
					Command: "backup",
					Job:     jobName,
					Stage:   "cleanup",
					Fields:  map[string]any{"error": safeErrorText(removeErr, secrets)},
				})
			}
		}
	}()
	s.write(logging.Event{
		Time:    started,
		Level:   logging.InfoLevel,
		Message: "backup staged",
		Command: "backup",
		Job:     jobName,
		Stage:   "temporary_storage",
		Fields:  map[string]any{"size": artifact.Size},
	})

	storageOptions, resolveErr := s.Resolve.Storage(job)
	if resolveErr != nil {
		return Result{}, stageError("configuration", resolveErr)
	}
	secrets = appendSecret(secrets, storageOptions.Credentials.AccessKeyID)
	secrets = appendSecret(secrets, storageOptions.Credentials.SecretAccessKey)
	secrets = appendSecret(secrets, storageOptions.Credentials.SessionToken)

	store, storeErr := s.Stores.New(ctx, storageOptions)
	if storeErr != nil {
		return Result{}, stageError("s3_upload", storeErr)
	}
	objectKey := storage.ObjectKey(job.S3.Prefix, jobName, started)
	s.write(logging.Event{
		Time:    started,
		Level:   logging.InfoLevel,
		Message: "backup upload started",
		Command: "backup",
		Job:     jobName,
		Stage:   "s3_upload",
		Fields:  map[string]any{"object_key": objectKey, "size": artifact.Size},
	})
	if _, uploadErr := store.UploadFile(ctx, job.S3.Bucket, objectKey, artifact.Path); uploadErr != nil {
		return Result{}, stageError("s3_upload", uploadErr)
	}

	result = Result{Job: jobName, ObjectKey: objectKey, Size: artifact.Size}
	return result, nil
}

func stageError(stageName string, err error) error {
	return &StageError{Stage: stageName, Err: err}
}

func errorStageOf(err error) string {
	for err != nil {
		if stageErr, ok := err.(*StageError); ok {
			return stageErr.Stage
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			break
		}
		err = unwrapper.Unwrap()
	}
	return "unknown"
}

func (s *Service) now() func() time.Time {
	if s.Now != nil {
		return s.Now
	}
	return time.Now
}

func (s *Service) write(event logging.Event) {
	if s.Log != nil {
		_ = s.Log.Write(event)
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
