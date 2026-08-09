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
