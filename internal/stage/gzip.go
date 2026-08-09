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

// GzipStager stores callback output in a private gzip file.
type GzipStager struct {
	removeFile func(string) error
}

// Stage writes a gzip-compressed artifact in dir. Every unsuccessful staging
// attempt removes its partial file.
func (s GzipStager) Stage(ctx context.Context, dir string, write func(io.Writer) error) (artifact Artifact, err error) {
	if err := ctx.Err(); err != nil {
		return Artifact{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Artifact{}, fmt.Errorf("create staging directory: %w", err)
	}
	file, err := os.CreateTemp(dir, "ezdbbackup-*.sql.gz")
	if err != nil {
		return Artifact{}, fmt.Errorf("create staging file: %w", err)
	}
	path := file.Name()
	success := false
	defer func() {
		if !success {
			if removeErr := s.remove(path); removeErr != nil {
				err = errors.Join(err, fmt.Errorf("remove partial staging file: %w", removeErr))
			}
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return Artifact{}, fmt.Errorf("secure staging file: %w", err)
	}

	writer := gzip.NewWriter(file)
	writeErr := write(writer)
	gzipCloseErr := writer.Close()
	fileCloseErr := file.Close()
	if writeErr != nil {
		return Artifact{}, writeErr
	}
	if gzipCloseErr != nil {
		return Artifact{}, fmt.Errorf("close gzip staging file: %w", gzipCloseErr)
	}
	if fileCloseErr != nil {
		return Artifact{}, fmt.Errorf("close staging file: %w", fileCloseErr)
	}
	if err := ctx.Err(); err != nil {
		return Artifact{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return Artifact{}, fmt.Errorf("stat staging file: %w", err)
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
