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

// Artifact identifies a completed temporary backup artifact. The unexported
// identity fields bind later opens and removal to the exact entry finalized by
// Stage; callers cannot construct a trusted artifact from a path alone.
type Artifact struct {
	Path string
	Size int64

	parentPath   string
	workName     string
	fileName     string
	parentDevice uint64
	parentInode  uint64
	workDevice   uint64
	workInode    uint64
	device       uint64
	inode        uint64
	links        uint64
	mode         uint32
}

// Stager writes backup output to a temporary artifact, reopens the exact
// finalized entry for upload, and removes only that entry.
type Stager interface {
	Stage(context.Context, string, func(io.Writer) error) (Artifact, error)
	Open(Artifact) (*os.File, error)
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

// GzipStager stores callback output in a private gzip file inside a unique
// mode-0700 directory beneath the configured staging parent.
type GzipStager struct {
	removeFile func(string) error
	newWriter  func(io.Writer) gzipWriteCloser
}

// Stage writes a gzip-compressed artifact in dir. Every unsuccessful staging
// attempt removes its exact partial file and private work directory.
func (s GzipStager) Stage(ctx context.Context, dir string, write func(io.Writer) error) (artifact Artifact, err error) {
	if err := ctx.Err(); err != nil {
		return Artifact{}, stageError(FailureTemporaryStorage, err)
	}
	workspace, err := newWorkspace(dir)
	if err != nil {
		return Artifact{}, stageError(FailureTemporaryStorage, err)
	}
	defer workspace.close()

	file, fileStat, err := workspace.createFile()
	if err != nil {
		cleanupErr := workspace.removeDirectory()
		if cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		}
		return Artifact{}, stageError(FailureTemporaryStorage, err)
	}
	artifact = workspace.artifact(fileStat)
	cleanupArtifact := artifact
	success := false
	fileClosed := false
	defer func() {
		if success {
			return
		}
		if !fileClosed {
			if current, statErr := statFileDescriptor(file); statErr == nil {
				cleanupArtifact = workspace.artifact(current)
			}
			_ = file.Close()
		}
		var cleanupErr error
		if s.removeFile != nil {
			cleanupErr = s.removeFile(cleanupArtifact.Path)
			if cleanupErr == nil {
				cleanupErr = workspace.removeDirectory()
			}
		} else {
			cleanupErr = workspace.removeArtifact(cleanupArtifact, false)
		}
		if cleanupErr != nil {
			err = errors.Join(err, stageError(FailureTemporaryStorage, fmt.Errorf("remove partial staging artifact: %w", cleanupErr)))
		}
		artifact = Artifact{}
	}()

	writer := s.writer(file)
	writeErr := write(writer)
	gzipCloseErr := writer.Close()
	finalStat, statErr := statFileDescriptor(file)
	if statErr == nil {
		artifact = workspace.artifact(finalStat)
		cleanupArtifact = artifact
	}
	fileCloseErr := file.Close()
	fileClosed = true

	if writeErr != nil {
		if gzipCloseErr != nil {
			return Artifact{}, errors.Join(
				writeErr,
				stageError(FailureCompression, fmt.Errorf("close gzip staging file: %w", gzipCloseErr)),
			)
		}
		return Artifact{}, writeErr
	}
	if gzipCloseErr != nil {
		return Artifact{}, stageError(FailureCompression, fmt.Errorf("close gzip staging file: %w", gzipCloseErr))
	}
	if statErr != nil {
		return Artifact{}, stageError(FailureTemporaryStorage, fmt.Errorf("inspect staging file: %w", statErr))
	}
	if fileCloseErr != nil {
		return Artifact{}, stageError(FailureTemporaryStorage, fmt.Errorf("close staging file: %w", fileCloseErr))
	}
	if err := ctx.Err(); err != nil {
		return Artifact{}, stageError(FailureTemporaryStorage, err)
	}
	if err := workspace.verifyFinal(artifact); err != nil {
		return Artifact{}, stageError(FailureTemporaryStorage, err)
	}

	artifact.Size = finalStat.Size
	success = true
	return artifact, nil
}

// Open returns a new read-only descriptor only when the configured parent,
// private work directory, and artifact path still identify the exact staged
// objects recorded by Stage.
func (GzipStager) Open(artifact Artifact) (*os.File, error) {
	file, err := openVerifiedArtifact(artifact)
	if err != nil {
		return nil, stageError(FailureTemporaryStorage, err)
	}
	return file, nil
}

// Remove deletes only the exact staged artifact and then its private work
// directory. A missing, replaced, relinked, resized, or permission-broadened
// path is left untouched and reported as a safety failure.
func (GzipStager) Remove(artifact Artifact) error {
	if err := removeVerifiedArtifact(artifact); err != nil {
		return stageError(FailureTemporaryStorage, err)
	}
	return nil
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
