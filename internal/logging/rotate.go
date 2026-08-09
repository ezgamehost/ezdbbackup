package logging

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func rotateIfNeeded(path string, options RotationOptions, now time.Time) error {
	info, err := os.Stat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat active log: %w", err)
	}
	if err == nil && options.MaxSizeBytes > 0 && info.Size() >= options.MaxSizeBytes {
		if err := rotate(path, options); err != nil {
			return err
		}
	}
	if err := enforceRetention(path, options, now); err != nil {
		return err
	}
	return nil
}

func rotate(path string, options RotationOptions) error {
	if options.MaxFiles <= 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove active log during rotation: %w", err)
		}
		return nil
	}

	for suffix := options.MaxFiles - 1; suffix >= 1; suffix-- {
		for _, extension := range []string{"", ".gz"} {
			from := rotatedPath(path, suffix) + extension
			to := rotatedPath(path, suffix+1) + extension
			if err := renameIfExists(from, to); err != nil {
				return fmt.Errorf("shift rotated log: %w", err)
			}
		}
	}

	firstRotation := rotatedPath(path, 1)
	if err := os.Remove(firstRotation); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("replace first rotation: %w", err)
	}
	if err := os.Remove(firstRotation + ".gz"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("replace first compressed rotation: %w", err)
	}
	if err := os.Rename(path, firstRotation); err != nil {
		return fmt.Errorf("rotate active log: %w", err)
	}
	if options.Compress {
		if err := compressFile(firstRotation); err != nil {
			return fmt.Errorf("compress rotated log: %w", err)
		}
	}
	return nil
}

func renameIfExists(from, to string) error {
	if err := os.Rename(from, to); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func compressFile(sourcePath string) (returnErr error) {
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()

	temporary, err := os.CreateTemp(filepath.Dir(sourcePath), ".log-gzip-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		if returnErr != nil {
			temporary.Close()
			os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(logFileMode); err != nil {
		return err
	}

	compressed := gzip.NewWriter(temporary)
	if _, err := io.Copy(compressed, source); err != nil {
		compressed.Close()
		return err
	}
	if err := compressed.Close(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, sourcePath+".gz"); err != nil {
		return err
	}
	if err := os.Remove(sourcePath); err != nil {
		return err
	}
	return nil
}

func enforceRetention(path string, options RotationOptions, now time.Time) error {
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("read log directory for retention: %w", err)
	}
	prefix := filepath.Base(path) + "."
	type rotation struct {
		name    string
		suffix  int
		modTime time.Time
	}
	var retained []rotation
	for _, entry := range entries {
		suffix, ok := rotationSuffix(entry.Name(), prefix)
		if !ok {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat rotated log for retention: %w", err)
		}
		remove := options.MaxFiles <= 0 || suffix > options.MaxFiles
		if !remove && options.MaxAge > 0 {
			remove = now.Sub(info.ModTime()) > options.MaxAge
		}
		if remove {
			if err := os.Remove(filepath.Join(filepath.Dir(path), entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove rotated log: %w", err)
			}
			continue
		}
		retained = append(retained, rotation{name: entry.Name(), suffix: suffix, modTime: info.ModTime()})
	}
	if options.MaxFiles <= 0 || len(retained) <= options.MaxFiles {
		return nil
	}

	sort.Slice(retained, func(left, right int) bool {
		if retained[left].suffix != retained[right].suffix {
			return retained[left].suffix < retained[right].suffix
		}
		if !retained[left].modTime.Equal(retained[right].modTime) {
			return retained[left].modTime.After(retained[right].modTime)
		}
		return retained[left].name < retained[right].name
	})
	for _, rotation := range retained[options.MaxFiles:] {
		if err := os.Remove(filepath.Join(filepath.Dir(path), rotation.name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove rotated log over count limit: %w", err)
		}
	}
	return nil
}

func rotationSuffix(name, prefix string) (int, bool) {
	if !strings.HasPrefix(name, prefix) {
		return 0, false
	}
	suffix := strings.TrimPrefix(name, prefix)
	suffix = strings.TrimSuffix(suffix, ".gz")
	if suffix == "" {
		return 0, false
	}
	number, err := strconv.Atoi(suffix)
	if err != nil || number < 1 {
		return 0, false
	}
	return number, true
}

func rotatedPath(path string, suffix int) string {
	return path + "." + strconv.Itoa(suffix)
}

func appendLine(path string, line []byte, mode os.FileMode) error {
	return appendLineWithWriter(path, line, mode, (*os.File).Write)
}

func appendLineWithWriter(path string, line []byte, mode os.FileMode, write func(*os.File, []byte) (int, error)) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("open active log: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return fmt.Errorf("stat active log before append: %w", err)
	}
	written, writeErr := write(file, line)
	if written != len(line) {
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		if err := file.Truncate(info.Size()); err != nil {
			file.Close()
			return fmt.Errorf("append active log: %w", errors.Join(writeErr, fmt.Errorf("rollback partial append: %w", err)))
		}
	}
	closeErr := file.Close()
	if writeErr != nil {
		return fmt.Errorf("append active log: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close active log: %w", closeErr)
	}
	return nil
}
