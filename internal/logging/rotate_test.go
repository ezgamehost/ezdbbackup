package logging

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRotationUsesSizeLimitAndRetainsMaxFiles(t *testing.T) {
	dir := secureLogDir(t)
	logger, err := New(Options{
		Directory: dir,
		Rotation:  RotationOptions{MaxSizeBytes: 256, MaxFiles: 2},
	})
	if err != nil {
		t.Fatal(err)
	}

	for sequence := 0; sequence < 7; sequence++ {
		if err := logger.Write(Event{
			Time: time.Unix(int64(sequence), 0), Level: InfoLevel,
			Message: "rotation test", Command: "backup",
			Fields: map[string]any{"sequence": sequence, "payload": strings.Repeat("x", 200)},
		}); err != nil {
			t.Fatal(err)
		}
	}

	for _, name := range []string{"info.log", "info.log.1", "info.log.2"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
	}
	if matches, err := filepath.Glob(filepath.Join(dir, "info.log.[3-9]*")); err != nil {
		t.Fatal(err)
	} else if len(matches) != 0 {
		t.Fatalf("retained more than two rotations: %v", matches)
	}
}

func TestRotationDeletesFilesOlderThanMaxAgeUsingClock(t *testing.T) {
	dir := secureLogDir(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	logger, err := New(Options{
		Directory: dir,
		Rotation:  RotationOptions{MaxSizeBytes: 1 << 20, MaxFiles: 5, MaxAge: 24 * time.Hour},
	})
	if err != nil {
		t.Fatal(err)
	}
	logger.now = func() time.Time { return now }

	oldPath := filepath.Join(dir, "info.log.1")
	recentPath := filepath.Join(dir, "info.log.2")
	for _, path := range []string{oldPath, recentPath} {
		if err := os.WriteFile(path, []byte("rotated\n"), logFileMode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(oldPath, now.Add(-25*time.Hour), now.Add(-25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(recentPath, now.Add(-23*time.Hour), now.Add(-23*time.Hour)); err != nil {
		t.Fatal(err)
	}

	if err := logger.Write(Event{Time: now, Level: InfoLevel, Message: "cleanup", Command: "backup"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old rotation was not deleted: %v", err)
	}
	if _, err := os.Stat(recentPath); err != nil {
		t.Fatalf("recent rotation was deleted: %v", err)
	}
}

func TestRotationCompressesOnlyRotatedLogAndPreservesContents(t *testing.T) {
	dir := secureLogDir(t)
	logger, err := New(Options{
		Directory: dir,
		Rotation:  RotationOptions{MaxSizeBytes: 256, MaxFiles: 2, Compress: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	first := Event{
		Time: time.Unix(1, 0), Level: InfoLevel, Message: "first", Command: "backup",
		Fields: map[string]any{"payload": strings.Repeat("x", 256)},
	}
	if err := logger.Write(first); err != nil {
		t.Fatal(err)
	}
	if err := logger.Write(Event{Time: time.Unix(2, 0), Level: InfoLevel, Message: "second", Command: "backup"}); err != nil {
		t.Fatal(err)
	}

	rotatedPath := filepath.Join(dir, "info.log.1.gz")
	lines := readLogLines(t, rotatedPath)
	if len(lines) != 1 || lines[0]["message"] != "first" {
		t.Fatalf("rotated lines = %#v", lines)
	}
	if _, err := os.Stat(filepath.Join(dir, "info.log.gz")); !os.IsNotExist(err) {
		t.Fatalf("active log was compressed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "info.log.lock.gz")); !os.IsNotExist(err) {
		t.Fatalf("lock file was compressed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "info.log.lock")); err != nil {
		t.Fatalf("stable lock file missing: %v", err)
	}
}

func TestRotationMaxFilesCountsMixedCompressedHistory(t *testing.T) {
	dir := secureLogDir(t)
	options := Options{
		Directory: dir,
		Rotation:  RotationOptions{MaxSizeBytes: 1, MaxFiles: 2, Compress: true},
	}
	logger, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	writeMessage := func(message string) {
		t.Helper()
		if err := logger.Write(Event{Level: InfoLevel, Message: message, Command: "backup"}); err != nil {
			t.Fatal(err)
		}
	}
	writeMessage("A")
	writeMessage("B")

	logger.options.Rotation.Compress = false
	writeMessage("C")
	logger.options.Rotation.Compress = true
	writeMessage("D")

	paths, err := filepath.Glob(filepath.Join(dir, "info.log.[0-9]*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 {
		t.Fatalf("retained %d rotated files, want 2: %v", len(paths), paths)
	}
	seen := make(map[string]bool)
	for _, path := range paths {
		for _, line := range readLogLines(t, path) {
			seen[line["message"].(string)] = true
		}
	}
	if !seen["B"] || !seen["C"] || seen["A"] {
		t.Fatalf("retained messages = %v, want B and C", seen)
	}
}

func TestConcurrentWritesRemainAtomicWhileForcedRotationOccurs(t *testing.T) {
	dir := secureLogDir(t)
	options := Options{
		Directory: dir,
		Rotation:  RotationOptions{MaxSizeBytes: 1, MaxFiles: 20, Compress: true},
	}
	first, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	loggers := []*FileLogger{first, second}

	errors := make(chan error, 10)
	var group sync.WaitGroup
	for writer := 0; writer < 10; writer++ {
		group.Add(1)
		go func() {
			defer group.Done()
			event := Event{
				Time: time.Unix(int64(writer), 0), Level: InfoLevel,
				Message: "concurrent", Command: "backup",
				Fields: map[string]any{"writer": writer, "payload": strings.Repeat("x", 64)},
			}
			errors <- loggers[writer%len(loggers)].Write(event)
		}()
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}

	paths, err := filepath.Glob(filepath.Join(dir, "info.log*"))
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[int]bool)
	lineCount := 0
	for _, path := range paths {
		if strings.HasSuffix(path, ".lock") {
			continue
		}
		for _, line := range readLogLines(t, path) {
			writer, ok := line["writer"].(float64)
			if !ok {
				t.Fatalf("line has no numeric writer: %#v", line)
			}
			seen[int(writer)] = true
			lineCount++
		}
	}
	if lineCount != 10 || len(seen) != 10 {
		t.Fatalf("got %d complete lines from %d writers; files: %v", lineCount, len(seen), paths)
	}
}

func TestAppendLineRollsBackPartialWrite(t *testing.T) {
	path := filepath.Join(secureLogDir(t), "info.log")
	initial := []byte("{\"message\":\"before\"}\n")
	if err := os.WriteFile(path, initial, logFileMode); err != nil {
		t.Fatal(err)
	}
	brokenLine := []byte("{\"message\":\"broken\"}\n")
	err := appendLineWithWriter(path, brokenLine, logFileMode, func(file *os.File, line []byte) (int, error) {
		written, err := file.Write(line[:len(line)/2])
		if err != nil {
			return written, err
		}
		return written, io.ErrShortWrite
	})
	if err == nil {
		t.Fatal("partial write returned nil error")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(initial) {
		t.Fatalf("log after partial write = %q, want %q", got, initial)
	}

	if err := appendLine(path, []byte("{\"message\":\"after\"}\n"), logFileMode); err != nil {
		t.Fatal(err)
	}
	lines := readLogLines(t, path)
	if len(lines) != 2 || lines[0]["message"] != "before" || lines[1]["message"] != "after" {
		t.Fatalf("lines after recovery = %#v", lines)
	}
}

func readLogLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var reader io.Reader = file
	if strings.HasSuffix(path, ".gz") {
		gzipReader, err := gzip.NewReader(file)
		if err != nil {
			t.Fatal(err)
		}
		defer gzipReader.Close()
		reader = gzipReader
	}

	var lines []map[string]any
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		var line map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatalf("decode JSON line in %s: %v (line %q)", path, err, scanner.Text())
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(fmt.Errorf("scan %s: %w", path, err))
	}
	return lines
}
