package stage

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type closeErrorWriter struct {
	io.Writer
	err error
}

func (w closeErrorWriter) Close() error { return w.err }

type writeCloseErrorWriter struct {
	err error
}

func (w writeCloseErrorWriter) Write([]byte) (int, error) { return 0, w.err }
func (w writeCloseErrorWriter) Close() error              { return w.err }

type callbackTestError struct{ message string }

func (e *callbackTestError) Error() string { return e.message }

type closeTestError struct{ message string }

func (e *closeTestError) Error() string { return e.message }

// This fails if staging does not create a private gzip artifact, report its
// compressed size, or remove it on request.
func TestStageCreatesGzipArtifactAndRemoveDeletesIt(t *testing.T) {
	s := GzipStager{}
	artifact, err := s.Stage(context.Background(), t.TempDir(), func(w io.Writer) error {
		_, err := io.WriteString(w, "CREATE TABLE example(id INT);\n")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	assertMode(t, artifact.Path, 0o600)
	assertGzipContents(t, artifact.Path, "CREATE TABLE example(id INT);\n")
	info, err := os.Stat(artifact.Path)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Size != info.Size() {
		t.Fatalf("Artifact.Size = %d, want final compressed size %d", artifact.Size, info.Size())
	}
	if err := s.Remove(artifact); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(artifact.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("artifact still exists: %v", err)
	}
}

// This fails if callback errors leave a partial sensitive dump in the staging
// directory or return an apparently usable artifact.
func TestStageRemovesPartialArtifactWhenCallbackFails(t *testing.T) {
	dir := t.TempDir()
	callbackErr := errors.New("write failure")
	artifact, err := (GzipStager{}).Stage(context.Background(), dir, func(w io.Writer) error {
		if _, writeErr := io.WriteString(w, "partial dump"); writeErr != nil {
			return writeErr
		}
		return callbackErr
	})
	if !errors.Is(err, callbackErr) {
		t.Fatalf("Stage() error = %v, want callback error", err)
	}
	if artifact != (Artifact{}) {
		t.Fatalf("Stage() artifact = %#v, want zero value on failure", artifact)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "ezdbbackup-*.sql.gz"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("partial artifacts remain: %#v", matches)
	}
}

// This fails if Stage suppresses a cleanup failure and leaves the caller
// unable to detect that a sensitive partial artifact remains on disk.
func TestStageReportsCallbackAndPartialArtifactCleanupFailures(t *testing.T) {
	callbackErr := errors.New("write failure")
	cleanupErr := errors.New("remove failure")
	s := GzipStager{removeFile: func(string) error { return cleanupErr }}
	_, err := s.Stage(context.Background(), t.TempDir(), func(io.Writer) error {
		return callbackErr
	})
	if !errors.Is(err, callbackErr) {
		t.Fatalf("Stage() error = %v, want callback failure", err)
	}
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("Stage() error = %v, want cleanup failure", err)
	}
}

func TestStageClassifiesTemporaryStorageFailure(t *testing.T) {
	notDirectory := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notDirectory, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := (GzipStager{}).Stage(context.Background(), notDirectory, func(io.Writer) error { return nil })
	var stageErr *Error
	if !errors.As(err, &stageErr) {
		t.Fatalf("Stage() error type = %T, want *Error", err)
	}
	if stageErr.Kind != FailureTemporaryStorage {
		t.Fatalf("Stage() failure kind = %q, want %q", stageErr.Kind, FailureTemporaryStorage)
	}
}

func TestStageClassifiesCompressionFailure(t *testing.T) {
	compressionErr := errors.New("compressor close failed")
	s := GzipStager{newWriter: func(writer io.Writer) gzipWriteCloser {
		return closeErrorWriter{Writer: writer, err: compressionErr}
	}}

	_, err := s.Stage(context.Background(), t.TempDir(), func(writer io.Writer) error {
		_, writeErr := io.WriteString(writer, "database dump")
		return writeErr
	})
	var stageErr *Error
	if !errors.As(err, &stageErr) {
		t.Fatalf("Stage() error type = %T, want *Error", err)
	}
	if stageErr.Kind != FailureCompression {
		t.Fatalf("Stage() failure kind = %q, want %q", stageErr.Kind, FailureCompression)
	}
	if !errors.Is(err, compressionErr) {
		t.Fatalf("Stage() error = %v, want original compression cause", err)
	}
}

func TestStageClassifiesCompressionWriteFailureBeforeCallbackError(t *testing.T) {
	compressionErr := errors.New("compressor write failed")
	s := GzipStager{newWriter: func(io.Writer) gzipWriteCloser {
		return writeCloseErrorWriter{err: compressionErr}
	}}

	_, err := s.Stage(context.Background(), t.TempDir(), func(writer io.Writer) error {
		_, writeErr := io.WriteString(writer, "database dump")
		return writeErr
	})
	var stageErr *Error
	if !errors.As(err, &stageErr) || stageErr.Kind != FailureCompression {
		t.Fatalf("Stage() error = %v, want typed compression failure", err)
	}
	if !errors.Is(err, compressionErr) {
		t.Fatalf("Stage() error = %v, want original compression cause", err)
	}
}

func TestStagePreservesCallbackAsPrimaryWhenGzipCloseAlsoFails(t *testing.T) {
	callbackErr := &callbackTestError{message: "dump callback failed"}
	closeErr := &closeTestError{message: "gzip close failed"}
	s := GzipStager{newWriter: func(io.Writer) gzipWriteCloser {
		return closeErrorWriter{Writer: io.Discard, err: closeErr}
	}}

	_, err := s.Stage(context.Background(), t.TempDir(), func(io.Writer) error {
		return callbackErr
	})
	if !errors.Is(err, callbackErr) || !errors.Is(err, closeErr) {
		t.Fatalf("Stage() error = %v, want callback and close causes", err)
	}
	var gotCallback *callbackTestError
	if !errors.As(err, &gotCallback) || gotCallback != callbackErr {
		t.Fatalf("Stage() error = %v, want typed callback cause", err)
	}
	var gotClose *closeTestError
	if !errors.As(err, &gotClose) || gotClose != closeErr {
		t.Fatalf("Stage() error = %v, want typed close cause", err)
	}
	aggregate, ok := err.(interface{ Unwrap() []error })
	if !ok || len(aggregate.Unwrap()) != 2 || !errors.Is(aggregate.Unwrap()[0], callbackErr) {
		t.Fatalf("Stage() error = %#v, want callback as first aggregate branch", err)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("file mode = %04o, want %04o", got, want)
	}
}

func assertGzipContents(t *testing.T, path, want string) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != want {
		t.Fatalf("gzip content = %q, want %q", got, want)
	}
	if strings.Contains(path, "..") {
		t.Fatalf("unexpected artifact path %q", path)
	}
}
