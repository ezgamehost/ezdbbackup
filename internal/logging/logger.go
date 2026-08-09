package logging

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const (
	logDirectoryMode = 0o750
	logFileMode      = 0o640
)

type FileLogger struct {
	options Options
	now     func() time.Time
}

func New(options Options) (*FileLogger, error) {
	if options.Directory == "" {
		return nil, errors.New("log directory is required")
	}
	if err := os.MkdirAll(options.Directory, logDirectoryMode); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}

	files := []string{"error.log", "info.log"}
	if options.Debug {
		files = append(files, "debug.log")
	}
	for _, name := range files {
		path := filepath.Join(options.Directory, name)
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, logFileMode)
		if err != nil {
			return nil, fmt.Errorf("initialize %s: %w", name, err)
		}
		if err := file.Close(); err != nil {
			return nil, fmt.Errorf("initialize %s: %w", name, err)
		}
	}
	return &FileLogger{options: options, now: time.Now}, nil
}

func (logger *FileLogger) Write(event Event) error {
	name, write := logger.destination(event.Level)
	if !write {
		return nil
	}
	line, err := encodeEvent(event)
	if err != nil {
		return err
	}
	path := filepath.Join(logger.options.Directory, name)
	return logger.appendLocked(path, line)
}

func (logger *FileLogger) appendLocked(path string, line []byte) error {
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, logFileMode)
	if err != nil {
		return fmt.Errorf("open log lock: %w", err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock log: %w", err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)

	if err := rotateIfNeeded(path, logger.options.Rotation, logger.now()); err != nil {
		return err
	}
	return appendLine(path, line, logFileMode)
}

func (logger *FileLogger) destination(level Level) (string, bool) {
	switch level {
	case ErrorLevel:
		return "error.log", true
	case InfoLevel, WarnLevel:
		return "info.log", true
	case DebugLevel:
		return "debug.log", logger.options.Debug
	default:
		return "", false
	}
}

func encodeEvent(event Event) ([]byte, error) {
	object := redactFields(event.Fields)
	if object == nil {
		object = make(map[string]any)
	}
	object["timestamp"] = event.Time.UTC()
	object["level"] = event.Level
	object["message"] = event.Message
	object["command"] = event.Command
	if event.Job != "" {
		object["job"] = event.Job
	}
	if event.Stage != "" {
		object["stage"] = event.Stage
	}
	line, err := json.Marshal(object)
	if err != nil {
		return nil, fmt.Errorf("encode log event: %w", err)
	}
	return append(line, '\n'), nil
}
