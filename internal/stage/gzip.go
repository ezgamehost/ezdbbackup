// Package stage creates and removes temporary backup artifacts.
package stage

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
)

// Artifact identifies a completed temporary backup artifact.
type Artifact struct {
	Path string
	Size int64
}

// Stager writes backup output to a temporary artifact.
type Stager interface {
	Stage(context.Context, string, func(io.Writer) error) (Artifact, error)
	Remove(Artifact) error
}

// FailureKind identifies which staging boundary failed.
type FailureKind string

const (
	FailureCompression      FailureKind = "compression"
	FailureTemporaryStorage FailureKind = "temporary_storage"
)

// Error classifies a staging failure and preserves its cause.
type Error struct {
	Kind FailureKind
	Err  error
}

func (e *Error) Error() string { return fmt.Sprintf("staging %s failed: %v", e.Kind, e.Err) }

func (e *Error) Unwrap() error { return e.Err }

type gzipWriteCloser interface {
	io.Writer
	Close() error
}

// GzipStager stores callback output in a private gzip file.
type GzipStager struct {
	removeFile func(string) error
	newWriter  func(io.Writer) gzipWriteCloser
}

// Stage writes a gzip-compressed artifact in dir. Every unsuccessful staging
// attempt removes its partial file.
func (s GzipStager) Stage(ctx context.Context, dir string, write func(io.Writer) error) (artifact Artifact, err error) {
	if err := ctx.Err(); err != nil {
		return Artifact{}, stageError(FailureTemporaryStorage, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Artifact{}, stageError(FailureTemporaryStorage, fmt.Errorf("create staging directory: %w", err))
	}
	file, err := os.CreateTemp(dir, "ezdbbackup-*.sql.gz")
	if err != nil {
		return Artifact{}, stageError(FailureTemporaryStorage, fmt.Errorf("create staging file: %w", err))
	}
	path := file.Name()
	success := false
	defer func() {
		if !success {
			if removeErr := s.remove(path); removeErr != nil {
				err = errors.Join(err, stageError(FailureTemporaryStorage, fmt.Errorf("remove partial staging file: %w", removeErr)))
			}
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return Artifact{}, stageError(FailureTemporaryStorage, fmt.Errorf("secure staging file: %w", err))
	}

	writer := s.writer(file)
	writeErr := write(writer)
	gzipCloseErr := writer.Close()
	fileCloseErr := file.Close()
	if gzipCloseErr != nil {
		return Artifact{}, stageError(FailureCompression, fmt.Errorf("close gzip staging file: %w", gzipCloseErr))
	}
	if writeErr != nil {
		return Artifact{}, writeErr
	}
	if fileCloseErr != nil {
		return Artifact{}, stageError(FailureTemporaryStorage, fmt.Errorf("close staging file: %w", fileCloseErr))
	}
	if err := ctx.Err(); err != nil {
		return Artifact{}, stageError(FailureTemporaryStorage, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return Artifact{}, stageError(FailureTemporaryStorage, fmt.Errorf("stat staging file: %w", err))
	}
	artifact = Artifact{Path: path, Size: info.Size()}
	success = true
	return artifact, nil
}

// Remove deletes a staged artifact.
func (GzipStager) Remove(artifact Artifact) error {
	return os.Remove(artifact.Path)
}

func (s GzipStager) remove(path string) error {
	if s.removeFile != nil {
		return s.removeFile(path)
	}
	return os.Remove(path)
}

func (s GzipStager) writer(dst io.Writer) gzipWriteCloser {
	if s.newWriter != nil {
		return s.newWriter(dst)
	}
	return gzip.NewWriter(dst)
}

func stageError(kind FailureKind, err error) error {
	return &Error{Kind: kind, Err: err}
}
