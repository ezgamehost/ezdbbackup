package cron

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const DefaultPath = "/etc/cron.d/ezdbbackup"

// Manager owns one marked system cron file. Path defaults to DefaultPath.
type Manager struct {
	Path   string
	rename func(oldPath, newPath string) error
}

// Install atomically writes content after verifying that both the proposed
// content and any existing destination belong to ezdbbackup.
func (m Manager) Install(content []byte) error {
	path := m.path()
	if !hasOwnershipMarker(content) {
		return fmt.Errorf("install cron %q: content lacks ownership marker", path)
	}
	if existing, err := os.ReadFile(path); err == nil {
		if !hasOwnershipMarker(existing) {
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
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("show cron %q: %w", path, err)
	}
	if !hasOwnershipMarker(content) {
		return nil, fmt.Errorf("show cron %q: file lacks ownership marker", path)
	}
	return content, nil
}

// Remove deletes the managed cron file. It succeeds if the file is absent.
func (m Manager) Remove() error {
	path := m.path()
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove cron %q: inspect existing file: %w", path, err)
	}
	if !hasOwnershipMarker(content) {
		return fmt.Errorf("remove cron %q: file lacks ownership marker", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove cron %q: %w", path, err)
	}
	return nil
}

func (m Manager) path() string {
	if m.Path == "" {
		return DefaultPath
	}
	return m.Path
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
