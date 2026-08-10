package cron

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestManagerInstallWritesExactContentAndMode(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "ezdbbackup")
	manager := Manager{Path: path}
	content := testManagedCron("0 2 * * * root /bin/true")

	if err := manager.Install(content); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("installed content = %q, want %q", got, content)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o644); got != want {
		t.Fatalf("installed mode = %04o, want %04o", got, want)
	}
}

func TestManagerInstallAtomicallyReplacesMarkedFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "ezdbbackup")
	oldContent := testManagedCron("old")
	newContent := testManagedCron("new")
	if err := os.WriteFile(path, oldContent, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if err := (Manager{Path: path}).Install(newContent); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	assertFileContent(t, path, newContent)
	assertDirectoryEntries(t, dir, "ezdbbackup")
}

func TestManagerInstallRefusesUnmarkedExistingFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "ezdbbackup")
	unmanaged := []byte("# maintained by an administrator\n")
	if err := os.WriteFile(path, unmanaged, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	err := (Manager{Path: path}).Install(testManagedCron("new"))
	if err == nil {
		t.Fatal("Install() error = nil, want non-nil")
	}
	assertFileContent(t, path, unmanaged)
	assertDirectoryEntries(t, dir, "ezdbbackup")
}

func TestManagerInstallRejectsUnmarkedNewContent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "ezdbbackup")
	if err := (Manager{Path: path}).Install([]byte("# not managed\n")); err == nil {
		t.Fatal("Install() error = nil, want non-nil")
	}
	assertDirectoryEntries(t, dir)
}

func TestManagerInstallRenameFailurePreservesPriorManagedFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "ezdbbackup")
	otherTemp := filepath.Join(dir, ".ezdbbackup.tmp-someone-else")
	oldContent := testManagedCron("old")
	if err := os.WriteFile(path, oldContent, 0o644); err != nil {
		t.Fatalf("WriteFile(existing) error = %v", err)
	}
	if err := os.WriteFile(otherTemp, []byte("leave me"), 0o600); err != nil {
		t.Fatalf("WriteFile(other temp) error = %v", err)
	}
	wantErr := errors.New("simulated rename failure")
	manager := Manager{
		Path: path,
		rename: func(_, _ string) error {
			return wantErr
		},
	}

	err := manager.Install(testManagedCron("new"))
	if !errors.Is(err, wantErr) {
		t.Fatalf("Install() error = %v, want wrapping %v", err, wantErr)
	}
	assertFileContent(t, path, oldContent)
	assertFileContent(t, otherTemp, []byte("leave me"))
	assertDirectoryEntries(t, dir, ".ezdbbackup.tmp-someone-else", "ezdbbackup")
}

func TestManagerShowReturnsExactManagedContent(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "ezdbbackup")
	content := testManagedCron("0 2 * * * root /bin/true")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	got, err := (Manager{Path: path}).Show()
	if err != nil {
		t.Fatalf("Show() error = %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("Show() = %q, want %q", got, content)
	}
}

func TestManagerShowRejectsMarkerOutsideHeader(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "ezdbbackup")
	content := []byte("SHELL=/bin/sh\n" + OwnershipMarker + "\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := (Manager{Path: path}).Show(); err == nil {
		t.Fatal("Show() error = nil, want non-nil")
	}
}

func TestManagerShowRejectsInexactMarker(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "ezdbbackup")
	content := []byte("# prefix " + OwnershipMarker + " suffix\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := (Manager{Path: path}).Show(); err == nil {
		t.Fatal("Show() error = nil, want non-nil")
	}
}

func TestManagerRemoveDeletesMarkedFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "ezdbbackup")
	if err := os.WriteFile(path, testManagedCron("entry"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := (Manager{Path: path}).Remove(); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat() error = %v, want os.ErrNotExist", err)
	}
}

func TestManagerRemoveRefusesUnmarkedFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "ezdbbackup")
	content := []byte("# administrator managed\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := (Manager{Path: path}).Remove(); err == nil {
		t.Fatal("Remove() error = nil, want non-nil")
	}
	assertFileContent(t, path, content)
}

func TestManagerRemoveSucceedsWhenFileIsAbsent(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "ezdbbackup")
	if err := (Manager{Path: path}).Remove(); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
}

func testManagedCron(body string) []byte {
	return []byte("# Generated by ezdbbackup. DO NOT EDIT.\n" + OwnershipMarker + "\n" + body + "\n")
}

func assertFileContent(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("ReadFile(%q) = %q, want %q", path, got, want)
	}
}

func assertDirectoryEntries(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != len(want) {
		t.Fatalf("ReadDir() returned %d entries, want %d: %v", len(entries), len(want), entries)
	}
	for i, entry := range entries {
		if entry.Name() != want[i] {
			t.Fatalf("ReadDir()[%d] = %q, want %q", i, entry.Name(), want[i])
		}
	}
}
