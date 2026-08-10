package cron

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
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
	assertDirectoryEntries(t, dir, ".ezdbbackup.lock", "ezdbbackup")
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
	assertDirectoryEntries(t, dir, ".ezdbbackup.lock", "ezdbbackup")
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
	assertDirectoryEntries(t, dir, ".ezdbbackup.lock", ".ezdbbackup.tmp-someone-else", "ezdbbackup")
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

	dir := t.TempDir()
	path := filepath.Join(dir, "ezdbbackup")
	if err := (Manager{Path: path}).Remove(); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	assertDirectoryEntries(t, dir)
}

func TestManagerOperationsRejectSymlinkAndHardlinkDestinations(t *testing.T) {
	t.Parallel()

	operations := []struct {
		name string
		run  func(Manager) error
	}{
		{name: "install", run: func(manager Manager) error { return manager.Install(testManagedCron("replacement")) }},
		{name: "show", run: func(manager Manager) error { _, err := manager.Show(); return err }},
		{name: "remove", run: func(manager Manager) error { return manager.Remove() }},
	}
	entryKinds := []struct {
		name string
		make func(t *testing.T, target, path string)
	}{
		{name: "symlink", make: func(t *testing.T, target, path string) {
			t.Helper()
			if err := os.Symlink(target, path); err != nil {
				t.Fatalf("Symlink() error = %v", err)
			}
		}},
		{name: "hardlink", make: func(t *testing.T, target, path string) {
			t.Helper()
			if err := os.Link(target, path); err != nil {
				t.Fatalf("Link() error = %v", err)
			}
		}},
	}

	for _, entryKind := range entryKinds {
		for _, operation := range operations {
			t.Run(entryKind.name+"/"+operation.name, func(t *testing.T) {
				t.Parallel()
				dir := t.TempDir()
				target := filepath.Join(dir, "target")
				path := filepath.Join(dir, "ezdbbackup")
				content := testManagedCron("original")
				if err := os.WriteFile(target, content, 0o644); err != nil {
					t.Fatalf("WriteFile() error = %v", err)
				}
				entryKind.make(t, target, path)

				if err := operation.run(Manager{Path: path}); err == nil {
					t.Fatalf("%s() error = nil, want unsafe-entry error", operation.name)
				}
				assertFileContent(t, target, content)
				if _, err := os.Lstat(path); err != nil {
					t.Fatalf("Lstat(destination) error = %v", err)
				}
			})
		}
	}
}

func TestManagerInstallRefusesDestinationChangedAfterInspection(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "ezdbbackup")
	oldPath := filepath.Join(dir, "old-ezdbbackup")
	unmanaged := []byte("# replacement owned by someone else\n")
	if err := os.WriteFile(path, testManagedCron("original"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	manager := Manager{
		Path: path,
		beforeMutation: func() {
			if err := os.Rename(path, oldPath); err != nil {
				t.Errorf("Rename() error = %v", err)
				return
			}
			if err := os.WriteFile(path, unmanaged, 0o644); err != nil {
				t.Errorf("WriteFile(replacement) error = %v", err)
			}
		},
	}

	if err := manager.Install(testManagedCron("new")); err == nil {
		t.Fatal("Install() error = nil, want changed-destination error")
	}
	assertFileContent(t, path, unmanaged)
}

func TestManagerRemoveRefusesDestinationChangedAfterInspection(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "ezdbbackup")
	oldPath := filepath.Join(dir, "old-ezdbbackup")
	unmanaged := []byte("# replacement owned by someone else\n")
	if err := os.WriteFile(path, testManagedCron("original"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	manager := Manager{
		Path: path,
		beforeMutation: func() {
			if err := os.Rename(path, oldPath); err != nil {
				t.Errorf("Rename() error = %v", err)
				return
			}
			if err := os.WriteFile(path, unmanaged, 0o644); err != nil {
				t.Errorf("WriteFile(replacement) error = %v", err)
			}
		},
	}

	if err := manager.Remove(); err == nil {
		t.Fatal("Remove() error = nil, want changed-destination error")
	}
	assertFileContent(t, path, unmanaged)
}

func TestManagerMutationsRefuseOwnershipChangedInPlaceAfterInspection(t *testing.T) {
	t.Parallel()

	operations := []struct {
		name string
		run  func(Manager) error
	}{
		{name: "install", run: func(manager Manager) error { return manager.Install(testManagedCron("new")) }},
		{name: "remove", run: func(manager Manager) error { return manager.Remove() }},
	}
	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "ezdbbackup")
			unmanaged := []byte("# ownership marker removed in place\n")
			if err := os.WriteFile(path, testManagedCron("original"), 0o644); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			manager := Manager{
				Path: path,
				beforeMutation: func() {
					if err := os.WriteFile(path, unmanaged, 0o644); err != nil {
						t.Errorf("WriteFile(replacement) error = %v", err)
					}
				},
			}

			if err := operation.run(manager); err == nil {
				t.Fatalf("%s() error = nil, want changed-ownership error", operation.name)
			}
			assertFileContent(t, path, unmanaged)
		})
	}
}

func TestManagerRemoveTreatsFinalNotExistAsSuccess(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "ezdbbackup")
	if err := os.WriteFile(path, testManagedCron("entry"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	manager := Manager{
		Path: path,
		remove: func(removePath string) error {
			if err := os.Remove(removePath); err != nil {
				return err
			}
			return &os.PathError{Op: "remove", Path: removePath, Err: os.ErrNotExist}
		},
	}

	if err := manager.Remove(); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Lstat() error = %v, want os.ErrNotExist", err)
	}
}

func TestManagerConcurrentInstallThenRemoveIsSerialized(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "ezdbbackup")
	if err := os.WriteFile(path, testManagedCron("original"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	renameEntered := make(chan struct{})
	releaseRename := make(chan struct{})
	install := Manager{
		Path: path,
		rename: func(oldPath, newPath string) error {
			close(renameEntered)
			<-releaseRename
			return os.Rename(oldPath, newPath)
		},
	}
	installDone := make(chan error, 1)
	go func() { installDone <- install.Install(testManagedCron("installed")) }()
	<-renameEntered

	removeBlocked := make(chan struct{})
	remove := Manager{Path: path, flock: expectContendedFlock(removeBlocked)}
	removeDone := make(chan error, 1)
	go func() { removeDone <- remove.Remove() }()
	assertOperationContendsOnLock(t, removeBlocked, removeDone)

	close(releaseRename)
	if err := <-installDone; err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if err := <-removeDone; err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Lstat() error = %v, want os.ErrNotExist", err)
	}
}

func TestManagerRemoveWaitsForFirstInstallWhoseDestinationIsStillAbsent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "ezdbbackup")
	renameEntered := make(chan struct{})
	releaseRename := make(chan struct{})
	install := Manager{
		Path: path,
		rename: func(oldPath, newPath string) error {
			close(renameEntered)
			<-releaseRename
			return os.Rename(oldPath, newPath)
		},
	}
	installDone := make(chan error, 1)
	go func() { installDone <- install.Install(testManagedCron("installed")) }()
	<-renameEntered

	removeBlocked := make(chan struct{})
	remove := Manager{Path: path, flock: expectContendedFlock(removeBlocked)}
	removeDone := make(chan error, 1)
	go func() { removeDone <- remove.Remove() }()
	assertOperationContendsOnLock(t, removeBlocked, removeDone)

	close(releaseRename)
	if err := <-installDone; err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if err := <-removeDone; err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Lstat() error = %v, want os.ErrNotExist", err)
	}
}

func TestManagerConcurrentRemoveThenInstallIsSerialized(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "ezdbbackup")
	if err := os.WriteFile(path, testManagedCron("original"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	removeEntered := make(chan struct{})
	releaseRemove := make(chan struct{})
	remove := Manager{
		Path: path,
		remove: func(removePath string) error {
			close(removeEntered)
			<-releaseRemove
			return os.Remove(removePath)
		},
	}
	removeDone := make(chan error, 1)
	go func() { removeDone <- remove.Remove() }()
	<-removeEntered

	installBlocked := make(chan struct{})
	install := Manager{Path: path, flock: expectContendedFlock(installBlocked)}
	installDone := make(chan error, 1)
	go func() { installDone <- install.Install(testManagedCron("installed")) }()
	assertOperationContendsOnLock(t, installBlocked, installDone)

	close(releaseRemove)
	if err := <-removeDone; err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if err := <-installDone; err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	assertFileContent(t, path, testManagedCron("installed"))
}

func expectContendedFlock(blocked chan<- struct{}) func(int, int) error {
	return func(fd, how int) error {
		err := unix.Flock(fd, how|unix.LOCK_NB)
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			close(blocked)
			return unix.Flock(fd, how)
		}
		if err != nil {
			return err
		}
		return errors.New("mutation lock was not held by the active operation")
	}
}

func assertOperationContendsOnLock(t *testing.T, blocked <-chan struct{}, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("concurrent operation did not contend on the active mutation lock: %v", err)
	case <-blocked:
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
