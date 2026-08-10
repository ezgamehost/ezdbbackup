package stage

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

type closeWritingWriter struct{ destination io.Writer }

func (w closeWritingWriter) Write(value []byte) (int, error) { return len(value), nil }
func (w closeWritingWriter) Close() error {
	_, err := w.destination.Write([]byte("gzip trailer"))
	return err
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(value []byte) (int, error) { return f(value) }

type callbackTestError struct{ message string }

func (e *callbackTestError) Error() string { return e.message }

type closeTestError struct{ message string }

func (e *closeTestError) Error() string { return e.message }

// This fails if staging does not create a private gzip artifact, report its
// compressed size, or remove it on request.
func TestStageCreatesGzipArtifactAndRemoveDeletesIt(t *testing.T) {
	s := GzipStager{}
	parent := secureTempDir(t)
	artifact, err := s.Stage(context.Background(), parent, func(w io.Writer) error {
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
	workDirectory := filepath.Dir(artifact.Path)
	if filepath.Dir(workDirectory) != parent {
		t.Fatalf("artifact path = %q, want one private work directory beneath %q", artifact.Path, parent)
	}
	assertMode(t, workDirectory, 0o700)
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("artifact stat type = %T, want *syscall.Stat_t", info.Sys())
	}
	if artifact.device != uint64(stat.Dev) || artifact.inode != stat.Ino || artifact.links != 1 {
		t.Fatalf("artifact identity = dev:%d inode:%d links:%d, want dev:%d inode:%d links:1", artifact.device, artifact.inode, artifact.links, stat.Dev, stat.Ino)
	}
	if err := s.Remove(artifact); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(artifact.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("artifact still exists: %v", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("staging parent entries after Remove() = %v, want none", entries)
	}
}

// This fails if callback errors leave a partial sensitive dump in the staging
// directory or return an apparently usable artifact.
func TestStageRemovesPartialArtifactWhenCallbackFails(t *testing.T) {
	dir := secureTempDir(t)
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
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("partial staging entries remain: %#v", entries)
	}
}

// This fails if upload callers cannot obtain a fresh descriptor that is
// verified against the exact file finalized by Stage.
func TestOpenReturnsVerifiedStagedDescriptor(t *testing.T) {
	s, artifact := createArtifact(t, "verified database dump")
	file, err := s.Open(artifact)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
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
	if got := string(data); got != "verified database dump" {
		t.Fatalf("verified descriptor contents = %q, want original dump", got)
	}
}

// This fails if a path substitution between Stage and Open can cause a
// symlink, hardlink, or different regular file to be uploaded.
func TestOpenRejectsArtifactPathSubstitution(t *testing.T) {
	tests := []struct {
		name    string
		replace func(*testing.T, Artifact)
	}{
		{
			name: "symbolic link",
			replace: func(t *testing.T, artifact Artifact) {
				replacement := filepath.Join(t.TempDir(), "replacement")
				if err := os.WriteFile(replacement, []byte("attacker symlink contents"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(artifact.Path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(replacement, artifact.Path); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "different regular file",
			replace: func(t *testing.T, artifact Artifact) {
				if err := os.Rename(artifact.Path, artifact.Path+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(artifact.Path, []byte("attacker regular contents"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "hardlink to different file",
			replace: func(t *testing.T, artifact Artifact) {
				replacement := filepath.Join(t.TempDir(), "replacement")
				if err := os.WriteFile(replacement, []byte("attacker hardlink contents"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(artifact.Path); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(replacement, artifact.Path); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, artifact := createArtifact(t, "original staged dump")
			test.replace(t, artifact)

			file, err := s.Open(artifact)
			if err == nil {
				_ = file.Close()
				t.Fatal("Open() error = nil, want identity-safety failure")
			}
			if !strings.Contains(err.Error(), "staging") {
				t.Fatalf("Open() error = %v, want bounded staging safety error", err)
			}
			if removeErr := s.Remove(artifact); removeErr == nil {
				t.Fatal("Remove() error = nil, want refusal to unlink replacement")
			}
			if _, statErr := os.Lstat(artifact.Path); statErr != nil {
				t.Fatalf("replacement was removed: %v", statErr)
			}
		})
	}
}

// This fails if adding another hardlink or changing the exact staged size can
// evade the recorded single-link/size identity checks.
func TestOpenRejectsChangedLinkCountSizeAndMode(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, Artifact)
	}{
		{
			name: "second hardlink",
			mutate: func(t *testing.T, artifact Artifact) {
				if err := os.Link(artifact.Path, artifact.Path+".second-link"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "changed size",
			mutate: func(t *testing.T, artifact Artifact) {
				file, err := os.OpenFile(artifact.Path, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := file.WriteString("changed"); err != nil {
					_ = file.Close()
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "broadened mode",
			mutate: func(t *testing.T, artifact Artifact) {
				if err := os.Chmod(artifact.Path, 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "special permission bit",
			mutate: func(t *testing.T, artifact Artifact) {
				if err := os.Chmod(artifact.Path, 0o600|os.ModeSetuid); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, artifact := createArtifact(t, "original staged dump")
			test.mutate(t, artifact)
			file, err := s.Open(artifact)
			if err == nil {
				_ = file.Close()
				t.Fatal("Open() error = nil, want changed identity rejected")
			}
			if removeErr := s.Remove(artifact); removeErr == nil {
				t.Fatal("Remove() error = nil, want changed identity left untouched")
			}
			if _, statErr := os.Lstat(artifact.Path); statErr != nil {
				t.Fatalf("changed original was removed: %v", statErr)
			}
		})
	}
}

// This fails if replacing the path after a verified open changes the bytes
// read from the descriptor or cleanup deletes the new path entry.
func TestVerifiedDescriptorSurvivesLaterPathReplacement(t *testing.T) {
	s, artifact := createArtifact(t, "original staged dump")
	file, err := s.Open(artifact)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := os.Rename(artifact.Path, artifact.Path+".original"); err != nil {
		t.Fatal(err)
	}
	const replacement = "unrelated replacement"
	if err := os.WriteFile(artifact.Path, []byte(replacement), 0o600); err != nil {
		t.Fatal(err)
	}

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
	if got := string(data); got != "original staged dump" {
		t.Fatalf("verified descriptor contents = %q, want original", got)
	}
	if err := s.Remove(artifact); err == nil {
		t.Fatal("Remove() error = nil, want path-replacement safety error")
	}
	if got, err := os.ReadFile(artifact.Path); err != nil || string(got) != replacement {
		t.Fatalf("replacement after Remove() = %q, %v; want untouched", got, err)
	}
}

// This fails if Stage relies only on prior CLI validation and can be invoked
// directly against a shared non-sticky parent.
func TestStageRejectsUnsafeSharedParentAndAllowsStickyParent(t *testing.T) {
	unsafeParent := t.TempDir()
	if err := os.Chmod(unsafeParent, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := (GzipStager{}).Stage(context.Background(), unsafeParent, func(io.Writer) error { return nil }); err == nil {
		t.Fatal("Stage(shared non-sticky) error = nil")
	}
	privateTarget := filepath.Join(unsafeParent, "private")
	if err := os.Mkdir(privateTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := (GzipStager{}).Stage(context.Background(), privateTarget, func(io.Writer) error { return nil }); err == nil {
		t.Fatal("Stage(private target beneath shared non-sticky ancestor) error = nil")
	}

	stickyParent := t.TempDir()
	if err := os.Chmod(stickyParent, os.ModeSticky|0o777); err != nil {
		t.Fatal(err)
	}
	s := GzipStager{}
	artifact, err := s.Stage(context.Background(), stickyParent, func(writer io.Writer) error {
		_, err := io.WriteString(writer, "dump")
		return err
	})
	if err != nil {
		t.Fatalf("Stage(sticky parent) error = %v", err)
	}
	if err := s.Remove(artifact); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafeParent, os.ModeSticky|0o777); err != nil {
		t.Fatal(err)
	}
	artifact, err = s.Stage(context.Background(), privateTarget, func(writer io.Writer) error {
		_, err := io.WriteString(writer, "dump beneath sticky ancestor")
		return err
	})
	if err != nil {
		t.Fatalf("Stage(private target beneath sticky ancestor) error = %v", err)
	}
	if err := s.Remove(artifact); err != nil {
		t.Fatal(err)
	}
}

func TestStageCreatesMissingParentByVerifiedComponentsAndRejectsSymlinkComponent(t *testing.T) {
	stickyParent := t.TempDir()
	if err := os.Chmod(stickyParent, os.ModeSticky|0o777); err != nil {
		t.Fatal(err)
	}
	nestedParent := filepath.Join(stickyParent, "nested", "stage")
	s := GzipStager{}
	artifact, err := s.Stage(context.Background(), nestedParent, func(writer io.Writer) error {
		_, err := io.WriteString(writer, "dump")
		return err
	})
	if err != nil {
		t.Fatalf("Stage(missing nested parent) error = %v", err)
	}
	assertMode(t, filepath.Join(stickyParent, "nested"), 0o700)
	assertMode(t, nestedParent, 0o700)
	if err := s.Remove(artifact); err != nil {
		t.Fatal(err)
	}

	secureParent := secureTempDir(t)
	symlinkTarget := secureTempDir(t)
	if err := os.Symlink(symlinkTarget, filepath.Join(secureParent, "link")); err != nil {
		t.Fatal(err)
	}
	throughSymlink := filepath.Join(secureParent, "link", "must-not-create")
	if _, err := s.Stage(context.Background(), throughSymlink, func(io.Writer) error { return nil }); err == nil {
		t.Fatal("Stage(symlink component) error = nil")
	}
	if _, err := os.Stat(filepath.Join(symlinkTarget, "must-not-create")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stage created directory through symlink component: %v", err)
	}
}

// This fails if Stage suppresses a cleanup failure and leaves the caller
// unable to detect that a sensitive partial artifact remains on disk.
func TestStageReportsCallbackAndPartialArtifactCleanupFailures(t *testing.T) {
	callbackErr := errors.New("write failure")
	cleanupErr := errors.New("remove failure")
	s := GzipStager{removeFile: func(string) error { return cleanupErr }}
	_, err := s.Stage(context.Background(), secureTempDir(t), func(io.Writer) error {
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

	_, err := s.Stage(context.Background(), secureTempDir(t), func(writer io.Writer) error {
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

	_, err := s.Stage(context.Background(), secureTempDir(t), func(writer io.Writer) error {
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

func TestStageReturnsTypedCompressionWriteErrorToDumpCallback(t *testing.T) {
	compressionErr := errors.New("compressor write failed")
	s := GzipStager{newWriter: func(io.Writer) gzipWriteCloser {
		return writeCloseErrorWriter{err: compressionErr}
	}}
	var callbackWriteErr error

	_, err := s.Stage(context.Background(), secureTempDir(t), func(writer io.Writer) error {
		_, callbackWriteErr = io.WriteString(writer, "database dump")
		return callbackWriteErr
	})
	var callbackStageErr *Error
	if !errors.As(callbackWriteErr, &callbackStageErr) || callbackStageErr.Kind != FailureCompression {
		t.Fatalf("callback write error = %v, want typed compression failure", callbackWriteErr)
	}
	if !errors.Is(err, compressionErr) {
		t.Fatalf("Stage() error = %v, want original compression cause", err)
	}
}

func TestStageReturnsTypedTemporaryStorageErrorFromGzipDestination(t *testing.T) {
	storageErr := syscall.ENOSPC
	s := GzipStager{
		destinationWriter: func(io.Writer) io.Writer {
			return writerFunc(func([]byte) (int, error) { return 0, storageErr })
		},
		newWriter: func(destination io.Writer) gzipWriteCloser {
			return closeErrorWriter{Writer: destination}
		},
	}
	var callbackWriteErr error

	_, err := s.Stage(context.Background(), secureTempDir(t), func(writer io.Writer) error {
		_, callbackWriteErr = io.WriteString(writer, "database dump")
		return callbackWriteErr
	})
	var callbackStageErr *Error
	if !errors.As(callbackWriteErr, &callbackStageErr) || callbackStageErr.Kind != FailureTemporaryStorage {
		t.Fatalf("callback write error = %v, want typed temporary-storage failure", callbackWriteErr)
	}
	if !errors.Is(err, storageErr) {
		t.Fatalf("Stage() error = %v, want ENOSPC cause", err)
	}
}

func TestStagePreservesTemporaryStorageTypeWhenGzipCloseFlushesDestination(t *testing.T) {
	storageErr := syscall.ENOSPC
	s := GzipStager{
		destinationWriter: func(io.Writer) io.Writer {
			return writerFunc(func([]byte) (int, error) { return 0, storageErr })
		},
		newWriter: func(destination io.Writer) gzipWriteCloser {
			return closeWritingWriter{destination: destination}
		},
	}

	_, err := s.Stage(context.Background(), secureTempDir(t), func(writer io.Writer) error {
		_, writeErr := io.WriteString(writer, "database dump")
		return writeErr
	})
	var stageErr *Error
	if !errors.As(err, &stageErr) || stageErr.Kind != FailureTemporaryStorage {
		t.Fatalf("Stage() error = %v, want destination-owned temporary-storage failure", err)
	}
	if !errors.Is(err, storageErr) {
		t.Fatalf("Stage() error = %v, want ENOSPC cause", err)
	}
}

func TestStagePreservesCallbackAsPrimaryWhenGzipCloseAlsoFails(t *testing.T) {
	callbackErr := &callbackTestError{message: "dump callback failed"}
	closeErr := &closeTestError{message: "gzip close failed"}
	s := GzipStager{newWriter: func(io.Writer) gzipWriteCloser {
		return closeErrorWriter{Writer: io.Discard, err: closeErr}
	}}

	_, err := s.Stage(context.Background(), secureTempDir(t), func(io.Writer) error {
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

func createArtifact(t *testing.T, contents string) (GzipStager, Artifact) {
	t.Helper()
	s := GzipStager{}
	artifact, err := s.Stage(context.Background(), secureTempDir(t), func(writer io.Writer) error {
		_, err := io.WriteString(writer, contents)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, artifact
}

func secureTempDir(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return directory
}
