package cron

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const DefaultPath = "/etc/cron.d/ezdbbackup"

// Manager owns one marked system cron file. Path defaults to DefaultPath.
type Manager struct {
	Path           string
	rename         func(oldPath, newPath string) error
	remove         func(path string) error
	flock          func(fd int, how int) error
	beforeMutation func()
}

// Install atomically writes content after verifying that both the proposed
// content and any existing destination belong to ezdbbackup.
func (m Manager) Install(content []byte) (returnErr error) {
	path := m.path()
	if !hasOwnershipMarker(content) {
		return fmt.Errorf("install cron %q: content lacks ownership marker", path)
	}
	lock, err := m.acquireMutationLock()
	if err != nil {
		return fmt.Errorf("install cron %q: acquire lock: %w", path, err)
	}
	defer func() {
		if err := releaseMutationLock(lock); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("install cron %q: release lock: %w", path, err))
		}
	}()
	existing, err := inspectDestination(path)
	if err == nil {
		if !hasOwnershipMarker(existing.content) {
			return fmt.Errorf("install cron %q: existing file lacks ownership marker", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("install cron %q: inspect existing file: %w", path, err)
	}

	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("install cron %q: create temporary file: %w", path, err)
	}
	tempPath := temp.Name()
	tempOpen := true
	keepTemp := false
	defer func() {
		if tempOpen {
			_ = temp.Close()
		}
		if !keepTemp {
			_ = os.Remove(tempPath)
		}
	}()

	if _, err := io.Copy(temp, bytes.NewReader(content)); err != nil {
		return fmt.Errorf("install cron %q: write temporary file: %w", path, err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("install cron %q: sync temporary file: %w", path, err)
	}
	if err := temp.Chmod(0o644); err != nil {
		return fmt.Errorf("install cron %q: set temporary file mode: %w", path, err)
	}
	if err := temp.Close(); err != nil {
		tempOpen = false
		return fmt.Errorf("install cron %q: close temporary file: %w", path, err)
	}
	tempOpen = false
	if m.beforeMutation != nil {
		m.beforeMutation()
	}
	if err := verifyDestinationOwnership(path, existing); err != nil {
		return fmt.Errorf("install cron %q: %w", path, err)
	}

	rename := m.rename
	if rename == nil {
		rename = os.Rename
	}
	if err := rename(tempPath, path); err != nil {
		return fmt.Errorf("install cron %q: replace destination: %w", path, err)
	}
	keepTemp = true
	return nil
}

// Show returns the exact contents of the managed cron file.
func (m Manager) Show() ([]byte, error) {
	path := m.path()
	destination, err := inspectDestination(path)
	if err != nil {
		return nil, fmt.Errorf("show cron %q: %w", path, err)
	}
	if !hasOwnershipMarker(destination.content) {
		return nil, fmt.Errorf("show cron %q: file lacks ownership marker", path)
	}
	return destination.content, nil
}

// Remove deletes the managed cron file. It succeeds if the file is absent.
func (m Manager) Remove() (returnErr error) {
	path := m.path()
	lock, err := m.acquireMutationLock()
	if err != nil {
		return fmt.Errorf("remove cron %q: acquire lock: %w", path, err)
	}
	defer func() {
		if err := releaseMutationLock(lock); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("remove cron %q: release lock: %w", path, err))
		}
	}()
	destination, err := inspectDestination(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove cron %q: inspect existing file: %w", path, err)
	}
	if !hasOwnershipMarker(destination.content) {
		return fmt.Errorf("remove cron %q: file lacks ownership marker", path)
	}
	if m.beforeMutation != nil {
		m.beforeMutation()
	}
	if err := verifyDestinationOwnership(path, destination); err != nil {
		return fmt.Errorf("remove cron %q: %w", path, err)
	}
	remove := m.remove
	if remove == nil {
		remove = os.Remove
	}
	if err := remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove cron %q: %w", path, err)
	}
	return nil
}

type inspectedDestination struct {
	content  []byte
	identity fileIdentity
}

type fileIdentity struct {
	device uint64
	inode  uint64
}

func inspectDestination(path string) (*inspectedDestination, error) {
	var pathStat unix.Stat_t
	if err := unix.Lstat(path, &pathStat); err != nil {
		return nil, err
	}
	if err := validateRegularSingleLink(path, &pathStat); err != nil {
		return nil, err
	}

	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()

	var openedStat unix.Stat_t
	if err := unix.Fstat(fd, &openedStat); err != nil {
		return nil, err
	}
	if err := validateRegularSingleLink(path, &openedStat); err != nil {
		return nil, err
	}
	if !sameFile(&pathStat, &openedStat) {
		return nil, fmt.Errorf("destination %q changed while it was inspected", path)
	}
	content, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	return &inspectedDestination{
		content:  content,
		identity: identityOf(&openedStat),
	}, nil
}

func validateRegularSingleLink(path string, stat *unix.Stat_t) error {
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("destination %q must be a regular file and not a symbolic link", path)
	}
	if stat.Nlink != 1 {
		return fmt.Errorf("destination %q must have exactly one hard link", path)
	}
	return nil
}

func identityOf(stat *unix.Stat_t) fileIdentity {
	return fileIdentity{device: uint64(stat.Dev), inode: stat.Ino}
}

func sameFile(left, right *unix.Stat_t) bool {
	return identityOf(left) == identityOf(right)
}

func verifyDestinationOwnership(path string, expected *inspectedDestination) error {
	if expected == nil {
		var current unix.Stat_t
		err := unix.Lstat(path, &current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect destination immediately before mutation: %w", err)
		}
		return fmt.Errorf("destination appeared after initial inspection")
	}
	current, err := inspectDestination(path)
	if err != nil {
		return fmt.Errorf("destination changed after initial inspection: %w", err)
	}
	if current.identity != expected.identity {
		return fmt.Errorf("destination changed after initial inspection")
	}
	if !hasOwnershipMarker(current.content) {
		return fmt.Errorf("destination ownership marker changed after initial inspection")
	}
	return nil
}

func (m Manager) path() string {
	if m.Path == "" {
		return DefaultPath
	}
	return m.Path
}

func (m Manager) acquireMutationLock() (*os.File, error) {
	path := m.path()
	// The hidden lock file is intentionally stable and never unlinked: removing
	// it could let concurrent processes lock different inodes. Its dot keeps
	// cron daemons from treating it as a schedule fragment.
	lockPath := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".lock")
	fd, err := unix.Open(lockPath, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	lock := os.NewFile(uintptr(fd), lockPath)
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = lock.Close()
		}
	}()

	var openedStat unix.Stat_t
	if err := unix.Fstat(fd, &openedStat); err != nil {
		return nil, err
	}
	if err := validateRegularSingleLink(lockPath, &openedStat); err != nil {
		return nil, err
	}
	flock := m.flock
	if flock == nil {
		flock = unix.Flock
	}
	for {
		err = flock(fd, unix.LOCK_EX)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		return nil, err
	}

	var pathStat unix.Stat_t
	if err := unix.Lstat(lockPath, &pathStat); err != nil {
		_ = unix.Flock(fd, unix.LOCK_UN)
		return nil, err
	}
	if err := validateRegularSingleLink(lockPath, &pathStat); err != nil {
		_ = unix.Flock(fd, unix.LOCK_UN)
		return nil, err
	}
	if !sameFile(&openedStat, &pathStat) {
		_ = unix.Flock(fd, unix.LOCK_UN)
		return nil, fmt.Errorf("lock file %q changed while acquiring lock", lockPath)
	}
	if err := unix.Fstat(fd, &openedStat); err != nil {
		_ = unix.Flock(fd, unix.LOCK_UN)
		return nil, err
	}
	if err := validateRegularSingleLink(lockPath, &openedStat); err != nil {
		_ = unix.Flock(fd, unix.LOCK_UN)
		return nil, err
	}

	closeOnError = false
	return lock, nil
}

func releaseMutationLock(lock *os.File) error {
	unlockErr := unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	closeErr := lock.Close()
	return errors.Join(unlockErr, closeErr)
}

func hasOwnershipMarker(content []byte) bool {
	marker := []byte(OwnershipMarker)
	for len(content) > 0 {
		line := content
		if newline := bytes.IndexByte(content, '\n'); newline >= 0 {
			line = content[:newline]
			content = content[newline+1:]
		} else {
			content = nil
		}
		if bytes.Equal(line, marker) {
			return true
		}
		if !bytes.HasPrefix(line, []byte("#")) {
			return false
		}
	}
	return false
}
