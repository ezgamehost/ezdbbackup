package logging

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteRoutesAndRedacts(t *testing.T) {
	dir := t.TempDir()
	logger, err := New(Options{
		Directory: dir,
		Debug:     true,
		Rotation: RotationOptions{
			MaxSizeBytes: 1 << 20,
			MaxFiles:     7,
			MaxAge:       30 * 24 * time.Hour,
			Compress:     true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	err = logger.Write(Event{
		Time: time.Unix(1, 0).UTC(), Level: ErrorLevel, Message: "upload failed",
		Command: "backup", Job: "production", Stage: "s3_upload",
		Fields: map[string]any{"password": "hidden", "attempt": 2},
	})
	if err != nil {
		t.Fatal(err)
	}

	line := readSingleJSONLine(t, filepath.Join(dir, "error.log"))
	if line["timestamp"] != "1970-01-01T00:00:01Z" || line["level"] != "error" ||
		line["message"] != "upload failed" || line["command"] != "backup" ||
		line["job"] != "production" || line["stage"] != "s3_upload" ||
		line["password"] != "[REDACTED]" || line["attempt"] != float64(2) {
		t.Fatalf("line = %#v", line)
	}
	assertLineCount(t, filepath.Join(dir, "info.log"), 0)
	assertLineCount(t, filepath.Join(dir, "debug.log"), 0)
}

func TestWriteRoutesEachLevelToOnlyItsDestination(t *testing.T) {
	dir := t.TempDir()
	logger, err := New(Options{Directory: dir, Debug: true})
	if err != nil {
		t.Fatal(err)
	}

	events := []Event{
		{Time: time.Unix(1, 0), Level: InfoLevel, Message: "info", Command: "backup"},
		{Time: time.Unix(2, 0), Level: WarnLevel, Message: "warn", Command: "backup"},
		{Time: time.Unix(3, 0), Level: DebugLevel, Message: "debug", Command: "backup"},
	}
	for _, event := range events {
		if err := logger.Write(event); err != nil {
			t.Fatal(err)
		}
	}

	assertLineCount(t, filepath.Join(dir, "info.log"), 2)
	assertLineCount(t, filepath.Join(dir, "debug.log"), 1)
	assertLineCount(t, filepath.Join(dir, "error.log"), 0)
}

func TestWriteOmitsDebugWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	logger, err := New(Options{Directory: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := logger.Write(Event{Level: DebugLevel, Message: "hidden", Command: "backup"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "debug.log")); !os.IsNotExist(err) {
		t.Fatalf("debug.log exists or stat returned unexpected error: %v", err)
	}
	assertLineCount(t, filepath.Join(dir, "info.log"), 0)
	assertLineCount(t, filepath.Join(dir, "error.log"), 0)
}

func TestWriteDoesNotMutateFields(t *testing.T) {
	dir := t.TempDir()
	logger, err := New(Options{Directory: dir})
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]any{
		"nested": map[string]any{"token": "keep-original"},
		"items":  []any{map[string]any{"password": "keep-original"}},
	}
	if err := logger.Write(Event{Level: InfoLevel, Message: "test", Command: "backup", Fields: fields}); err != nil {
		t.Fatal(err)
	}
	if got := fields["nested"].(map[string]any)["token"]; got != "keep-original" {
		t.Fatalf("nested token mutated to %v", got)
	}
	if got := fields["items"].([]any)[0].(map[string]any)["password"]; got != "keep-original" {
		t.Fatalf("slice member mutated to %v", got)
	}
}

func TestWriteRedactsKeysProducedByStructuredFieldValues(t *testing.T) {
	type credentials struct {
		Password string `json:"database_password"`
		Safe     string `json:"safe"`
	}
	dir := t.TempDir()
	logger, err := New(Options{Directory: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := logger.Write(Event{
		Level: InfoLevel, Message: "structured", Command: "backup",
		Fields: map[string]any{"database": credentials{Password: "hidden", Safe: "visible"}},
	}); err != nil {
		t.Fatal(err)
	}

	line := readSingleJSONLine(t, filepath.Join(dir, "info.log"))
	database := line["database"].(map[string]any)
	if database["database_password"] != "[REDACTED]" || database["safe"] != "visible" {
		t.Fatalf("database field = %#v", database)
	}
}

func readSingleJSONLine(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var line map[string]any
	if err := json.Unmarshal(data, &line); err != nil {
		t.Fatalf("decode %s: %v (data %q)", path, err, data)
	}
	return line
}

func assertLineCount(t *testing.T, path string, want int) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	got := 0
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		got++
		var line map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatalf("invalid JSON line in %s: %v", path, err)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s has %d lines, want %d", path, got, want)
	}
}
