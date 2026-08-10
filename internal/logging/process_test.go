package logging

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	processHelperEnvironment = "EZDBBACKUP_LOG_PROCESS_HELPER"
	processHelperDirectory   = "EZDBBACKUP_LOG_PROCESS_DIRECTORY"
	processHelperStart       = "EZDBBACKUP_LOG_PROCESS_START"
	processHelperWriter      = "EZDBBACKUP_LOG_PROCESS_WRITER"
	processHelperCount       = "EZDBBACKUP_LOG_PROCESS_COUNT"
	processWriterCount       = 6
	processEventCount        = 80
	processRotationBytes     = 4096
	processRotationFiles     = 64
)

func TestSeparateProcessesSerializeWritesAndRotation(t *testing.T) {
	directory := secureLogDir(t)
	start := filepath.Join(t.TempDir(), "start")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	type child struct {
		command *exec.Cmd
		output  bytes.Buffer
	}
	children := make([]child, processWriterCount)
	t.Cleanup(func() {
		cancel()
		for index := range children {
			if children[index].command == nil {
				continue
			}
			if process := children[index].command.Process; process != nil {
				_ = process.Kill()
			}
		}
	})
	for writer := range children {
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLoggingProcessHelper$", "-test.v")
		command.Env = processChildEnvironment(
			processHelperEnvironment+"=1",
			processHelperDirectory+"="+directory,
			processHelperStart+"="+start,
			processHelperWriter+"="+strconv.Itoa(writer),
			processHelperCount+"="+strconv.Itoa(processEventCount),
		)
		children[writer].command = command
		command.Stdout = &children[writer].output
		command.Stderr = &children[writer].output
		if err := command.Start(); err != nil {
			t.Fatalf("start writer %d: %v", writer, err)
		}
	}

	if err := os.WriteFile(start, []byte("start\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	type result struct {
		writer int
		err    error
	}
	results := make(chan result, len(children))
	for writer := range children {
		go func() {
			results <- result{writer: writer, err: children[writer].command.Wait()}
		}()
	}
	for range children {
		result := <-results
		if result.err != nil {
			t.Errorf("writer %d: %v\n%s", result.writer, result.err, children[result.writer].output.String())
		}
	}
	if t.Failed() {
		return
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("separate log writers did not finish within the bound: %v", err)
	}

	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]struct{}, processWriterCount*processEventCount)
	rotations := make(map[int]struct{})
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(directory, name)
		if name == "info.log" {
			assertProcessLogMetadata(t, path)
			collectProcessLines(t, path, seen)
			continue
		}
		suffix, compressed, ok := rotationSuffix(name, "info.log.")
		if !ok {
			continue
		}
		if !compressed {
			t.Errorf("rotation %q is not compressed", name)
		}
		if suffix < 1 || suffix > processRotationFiles {
			t.Errorf("rotation suffix %d is outside retention bound", suffix)
		}
		if _, duplicate := rotations[suffix]; duplicate {
			t.Errorf("duplicate rotation suffix %d", suffix)
		}
		rotations[suffix] = struct{}{}
		assertProcessLogMetadata(t, path)
		collectProcessLines(t, path, seen)
	}

	wantLines := processWriterCount * processEventCount
	if len(seen) != wantLines {
		t.Fatalf("retained %d unique complete process records, want %d", len(seen), wantLines)
	}
	if len(rotations) == 0 {
		t.Fatal("separate process writes did not exercise rotation")
	}
	for suffix := 1; suffix <= len(rotations); suffix++ {
		if _, ok := rotations[suffix]; !ok {
			t.Errorf("rotation history has a gap at suffix %d: %v", suffix, rotations)
		}
	}

	if err := os.RemoveAll(directory); err != nil {
		t.Fatalf("clean process logging fixture: %v", err)
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("process logging fixture still exists after cleanup: %v", err)
	}
}

func TestLoggingProcessHelper(t *testing.T) {
	if os.Getenv(processHelperEnvironment) != "1" {
		t.Skip("subprocess helper")
	}
	directory := requiredProcessEnvironment(t, processHelperDirectory)
	start := requiredProcessEnvironment(t, processHelperStart)
	writer := requiredProcessInteger(t, processHelperWriter)
	count := requiredProcessInteger(t, processHelperCount)

	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := os.Stat(start)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for process start marker")
		}
		time.Sleep(2 * time.Millisecond)
	}

	logger, err := New(Options{
		Directory: directory,
		Rotation: RotationOptions{
			MaxSizeBytes: processRotationBytes,
			MaxFiles:     processRotationFiles,
			Compress:     true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := strings.Repeat(string(rune('a'+writer)), 96)
	for sequence := 0; sequence < count; sequence++ {
		err := logger.Write(Event{
			Time:    time.Unix(int64(writer*count+sequence), 0).UTC(),
			Level:   InfoLevel,
			Message: "separate process record",
			Command: "backup",
			Fields: map[string]any{
				"writer":   writer,
				"sequence": sequence,
				"payload":  payload,
			},
		})
		if err != nil {
			t.Fatalf("write record %d: %v", sequence, err)
		}
	}
}

func requiredProcessEnvironment(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("%s is required", name)
	}
	return value
}

func requiredProcessInteger(t *testing.T, name string) int {
	t.Helper()
	value, err := strconv.Atoi(requiredProcessEnvironment(t, name))
	if err != nil || value < 0 {
		t.Fatalf("%s is invalid: %q", name, os.Getenv(name))
	}
	return value
}

func processChildEnvironment(overrides ...string) []string {
	names := map[string]bool{
		processHelperEnvironment: true,
		processHelperDirectory:   true,
		processHelperStart:       true,
		processHelperWriter:      true,
		processHelperCount:       true,
	}
	environment := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !names[name] {
			environment = append(environment, entry)
		}
	}
	return append(environment, overrides...)
}

func assertProcessLogMetadata(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o640 {
		t.Errorf("%s mode = %s, want regular 0640", path, info.Mode())
	}
}

func collectProcessLines(t *testing.T, path string, seen map[string]struct{}) {
	t.Helper()
	for _, line := range readLogLines(t, path) {
		if line["message"] != "separate process record" || line["command"] != "backup" {
			t.Fatalf("unexpected process log record in %s: %#v", path, line)
		}
		writer, writerOK := line["writer"].(float64)
		sequence, sequenceOK := line["sequence"].(float64)
		if !writerOK || !sequenceOK || writer < 0 || writer >= processWriterCount || sequence < 0 || sequence >= processEventCount {
			t.Fatalf("invalid process identity in %s: %#v", path, line)
		}
		wantPayload := strings.Repeat(string(rune('a'+int(writer))), 96)
		if line["payload"] != wantPayload {
			t.Fatalf("torn payload in %s: %#v", path, line)
		}
		key := fmt.Sprintf("%d/%d", int(writer), int(sequence))
		if _, duplicate := seen[key]; duplicate {
			t.Fatalf("duplicate process record %s", key)
		}
		seen[key] = struct{}{}
	}
}
