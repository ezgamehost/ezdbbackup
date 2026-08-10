package logging

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

const logDirectoryMode = 0o750

// MaxRotationFiles bounds configured history and all rotation work.
const MaxRotationFiles = 1000

type FileLogger struct {
	options               Options
	now                   func() time.Time
	directoryIdentity     logIdentity
	beforeMutation        func(operation, name string)
	gzipTemporaryName     func(sourceName string, attempt int) string
	rotationMutationCount atomic.Int64
}

func New(options Options) (*FileLogger, error) {
	if options.Directory == "" {
		return nil, errors.New("log directory is required")
	}
	if options.Rotation.MaxFiles > MaxRotationFiles {
		return nil, fmt.Errorf("max_files must not exceed %d", MaxRotationFiles)
	}
	directory, err := ensureBoundLogDirectory(options.Directory, options.Binding)
	if err != nil {
		return nil, fmt.Errorf("initialize log directory: %w", err)
	}
	defer directory.close()
	logger := &FileLogger{
		options:           options,
		now:               time.Now,
		directoryIdentity: directory.identity,
	}

	files := []string{"error.log", "info.log"}
	if options.Debug {
		files = append(files, "debug.log")
	}
	for _, name := range files {
		if err := logger.withFamilyLock(directory, name, true, func() error {
			active, _, err := directory.openFile(name, unix.O_WRONLY|unix.O_APPEND, true)
			if err != nil {
				return fmt.Errorf("initialize %s: %w", name, err)
			}
			if err := active.Close(); err != nil {
				return fmt.Errorf("initialize %s: close active log: %w", name, err)
			}
			if err := rotateIfNeeded(directory, name, options.Rotation, logger.now(), logger); err != nil {
				return fmt.Errorf("initialize %s: %w", name, err)
			}
			active, _, err = directory.openFile(name, unix.O_WRONLY|unix.O_APPEND, true)
			if err != nil {
				return fmt.Errorf("initialize %s after rotation: %w", name, err)
			}
			return active.Close()
		}); err != nil {
			return nil, err
		}
	}
	return logger, nil
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
	directory, err := reopenLogDirectory(logger.options.Directory, logger.directoryIdentity)
	if err != nil {
		return fmt.Errorf("open log directory: %w", err)
	}
	defer directory.close()
	return logger.withFamilyLock(directory, name, false, func() error {
		if err := rotateIfNeeded(directory, name, logger.options.Rotation, logger.now(), logger); err != nil {
			return err
		}
		return appendLineAt(directory, name, line, logger, nil)
	})
}

func (logger *FileLogger) withFamilyLock(directory *logDirectory, activeName string, create bool, fn func() error) (returnErr error) {
	// The pinned directory inode is the nonreplaceable coordination point. A
	// shared-group member can rename a visible per-family lock entry, but every
	// cooperating logger still serializes on this descriptor before touching it.
	if err := flockExclusive(directory.fd); err != nil {
		return fmt.Errorf("lock log directory: %w", err)
	}
	defer func() {
		returnErr = errors.Join(returnErr, unix.Flock(directory.fd, unix.LOCK_UN))
	}()

	lockName := activeName + ".lock"
	lock, snapshot, err := directory.openFile(lockName, unix.O_RDWR, create)
	if err != nil {
		return fmt.Errorf("open log lock: %w", err)
	}
	defer func() {
		returnErr = errors.Join(returnErr, lock.Close())
	}()
	if err := flockExclusive(int(lock.Fd())); err != nil {
		return fmt.Errorf("lock log: %w", err)
	}
	defer func() {
		returnErr = errors.Join(returnErr, unix.Flock(int(lock.Fd()), unix.LOCK_UN))
	}()
	if logger.beforeMutation != nil {
		logger.beforeMutation("lock_recheck", lockName)
	}
	if err := directory.verifyFile(lockName, snapshot); err != nil {
		return fmt.Errorf("stable log lock changed: %w", err)
	}
	var opened unix.Stat_t
	if err := unix.Fstat(int(lock.Fd()), &opened); err != nil {
		return fmt.Errorf("recheck log lock descriptor: %w", err)
	}
	if identityFromStat(opened) != snapshot.identity || opened.Nlink != 1 {
		return errors.New("stable log lock descriptor changed")
	}
	operationErr := fn()
	pathErr := directory.verifyFile(lockName, snapshot)
	var descriptor unix.Stat_t
	descriptorErr := unix.Fstat(int(lock.Fd()), &descriptor)
	if descriptorErr == nil && (identityFromStat(descriptor) != snapshot.identity || descriptor.Nlink != 1) {
		descriptorErr = errors.New("stable log lock descriptor changed during operation")
	}
	if pathErr != nil {
		pathErr = fmt.Errorf("stable log lock changed during operation: %w", pathErr)
	}
	return errors.Join(operationErr, pathErr, descriptorErr)
}

func flockExclusive(fd int) error {
	for {
		err := unix.Flock(fd, unix.LOCK_EX)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
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
