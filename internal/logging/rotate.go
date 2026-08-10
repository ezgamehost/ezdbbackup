package logging

import (
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type rotationFile struct {
	name       string
	suffix     int
	compressed bool
	snapshot   fileSnapshot
}

func rotateIfNeeded(directory *logDirectory, activeName string, options RotationOptions, now time.Time, logger *FileLogger) error {
	if options.MaxFiles > MaxRotationFiles {
		return fmt.Errorf("max_files must not exceed %d", MaxRotationFiles)
	}
	active, err := directory.statFile(activeName)
	if err != nil {
		return fmt.Errorf("stat active log: %w", err)
	}
	if options.MaxSizeBytes > 0 && active.size >= options.MaxSizeBytes {
		if err := rotate(directory, activeName, active, options, logger); err != nil {
			return err
		}
	}
	if err := enforceRetention(directory, activeName, options, now, logger); err != nil {
		return err
	}
	return nil
}

func rotate(directory *logDirectory, activeName string, active fileSnapshot, options RotationOptions, logger *FileLogger) error {
	if options.MaxFiles <= 0 {
		file, opened, err := directory.openFile(activeName, unix.O_WRONLY, false)
		if err != nil {
			return fmt.Errorf("open active log for reset: %w", err)
		}
		defer file.Close()
		if opened.identity != active.identity {
			return errors.New("active log changed before reset")
		}
		if logger.beforeMutation != nil {
			logger.beforeMutation("truncate_recheck", activeName)
		}
		if err := directory.verifyFile(activeName, active); err != nil {
			return fmt.Errorf("active log changed before reset: %w", err)
		}
		if err := file.Truncate(0); err != nil {
			return fmt.Errorf("reset active log: %w", err)
		}
		return nil
	}

	rotations, err := listRotations(directory, activeName)
	if err != nil {
		return err
	}
	sort.Slice(rotations, func(i, j int) bool {
		if rotations[i].suffix != rotations[j].suffix {
			return rotations[i].suffix > rotations[j].suffix
		}
		return rotations[i].name > rotations[j].name
	})
	for _, existing := range rotations {
		if existing.suffix >= options.MaxFiles {
			if err := safeUnlinkAt(directory, existing.name, existing.snapshot, logger); err != nil {
				return fmt.Errorf("prune oldest rotated log: %w", err)
			}
			continue
		}
		extension := ""
		if existing.compressed {
			extension = ".gz"
		}
		destination := rotatedName(activeName, existing.suffix+1) + extension
		if err := safeRenameAt(directory, existing.name, destination, existing.snapshot, logger); err != nil {
			return fmt.Errorf("shift rotated log: %w", err)
		}
	}

	first := rotatedName(activeName, 1)
	if err := safeRenameAt(directory, activeName, first, active, logger); err != nil {
		return fmt.Errorf("rotate active log: %w", err)
	}
	if options.Compress {
		rotated, err := directory.statFile(first)
		if err != nil {
			return fmt.Errorf("inspect first rotation: %w", err)
		}
		if err := compressFileAt(directory, first, rotated, logger); err != nil {
			return fmt.Errorf("compress rotated log: %w", err)
		}
	}
	return nil
}

func listRotations(directory *logDirectory, activeName string) ([]rotationFile, error) {
	fd, err := unix.Openat(directory.fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open log directory for enumeration: %w", err)
	}
	file := os.NewFile(uintptr(fd), directory.resolved)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("create log directory enumeration descriptor")
	}
	entries, readErr := file.ReadDir(-1)
	closeErr := file.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read log directory: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close log directory enumeration: %w", closeErr)
	}

	rotations := make([]rotationFile, 0)
	for _, entry := range entries {
		suffix, compressed, ok := rotationSuffix(entry.Name(), activeName+".")
		if !ok {
			continue
		}
		snapshot, err := directory.statFile(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("inspect rotated log %q: %w", entry.Name(), err)
		}
		rotations = append(rotations, rotationFile{
			name:       entry.Name(),
			suffix:     suffix,
			compressed: compressed,
			snapshot:   snapshot,
		})
	}
	return rotations, nil
}

func safeRenameAt(directory *logDirectory, source, destination string, expected fileSnapshot, logger *FileLogger) error {
	if err := validateLogName(source); err != nil {
		return err
	}
	if err := validateLogName(destination); err != nil {
		return err
	}
	exists, err := directory.entryExists(destination)
	if err != nil {
		return fmt.Errorf("inspect rotation destination: %w", err)
	}
	if exists {
		return fmt.Errorf("rotation destination %q unexpectedly exists", destination)
	}
	if logger.beforeMutation != nil {
		logger.beforeMutation("rename_recheck", source)
	}
	if err := directory.verifyFile(source, expected); err != nil {
		return fmt.Errorf("source changed before rename: %w", err)
	}
	if err := unix.Renameat2(directory.fd, source, directory.fd, destination, unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	if err := directory.verifyFile(destination, expected); err != nil {
		return fmt.Errorf("renamed log identity changed: %w", err)
	}
	logger.rotationMutationCount.Add(1)
	return nil
}

func safeUnlinkAt(directory *logDirectory, name string, expected fileSnapshot, logger *FileLogger) (returnErr error) {
	if err := validateLogName(name); err != nil {
		return err
	}
	if logger.beforeMutation != nil {
		logger.beforeMutation("remove_recheck", name)
	}
	if err := directory.verifyFile(name, expected); err != nil {
		return fmt.Errorf("candidate changed before removal: %w", err)
	}
	if logger.beforeMutation != nil {
		logger.beforeMutation("remove_after_verify", name)
	}
	quarantineFD, err := openQuarantineDirectory(directory)
	if err != nil {
		return fmt.Errorf("open protected log quarantine: %w", err)
	}
	defer func() {
		returnErr = errors.Join(returnErr, unix.Close(quarantineFD))
	}()
	quarantineName := ""
	for attempt := 0; attempt < 16; attempt++ {
		candidate, randomErr := randomQuarantineName()
		if randomErr != nil {
			return randomErr
		}
		renameErr := unix.Renameat2(directory.fd, name, quarantineFD, candidate, unix.RENAME_NOREPLACE)
		if renameErr == nil {
			quarantineName = candidate
			break
		}
		if !errors.Is(renameErr, unix.EEXIST) {
			return renameErr
		}
	}
	if quarantineName == "" {
		return errors.New("reserve unique protected quarantine name")
	}
	var moved unix.Stat_t
	statErr := unix.Fstatat(quarantineFD, quarantineName, &moved, unix.AT_SYMLINK_NOFOLLOW)
	if statErr == nil {
		statErr = directory.validateFile(quarantineName, moved)
	}
	if statErr == nil && !snapshotMatchesStat(expected, moved) {
		statErr = errors.New("quarantined candidate identity changed")
	}
	if statErr != nil {
		restoreErr := unix.Renameat2(quarantineFD, quarantineName, directory.fd, name, unix.RENAME_NOREPLACE)
		if restoreErr != nil {
			return fmt.Errorf("candidate changed after verification and was preserved in protected quarantine: %w", errors.Join(statErr, restoreErr))
		}
		return fmt.Errorf("candidate changed after verification: %w", statErr)
	}
	if err := unix.Unlinkat(quarantineFD, quarantineName, 0); err != nil {
		return err
	}
	logger.rotationMutationCount.Add(1)
	return nil
}

func openQuarantineDirectory(directory *logDirectory) (int, error) {
	name := ".ezdbbackup-quarantine-" + strconv.Itoa(os.Geteuid())
	created := false
	if err := unix.Mkdirat(directory.fd, name, 0o700); err == nil {
		created = true
	} else if !errors.Is(err, unix.EEXIST) {
		return -1, err
	}
	fd, err := unix.Openat(directory.fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = unix.Close(fd)
		}
	}()
	if created {
		if err := unix.Fchmod(fd, 0o700); err != nil {
			return -1, err
		}
	}
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		return -1, err
	}
	var entry unix.Stat_t
	if err := unix.Fstatat(directory.fd, name, &entry, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return -1, err
	}
	if !sameStatIdentity(opened, entry) {
		return -1, errors.New("log quarantine changed while opening")
	}
	if opened.Mode&unix.S_IFMT != unix.S_IFDIR || opened.Mode&0o7777 != 0o700 || opened.Uid != uint32(os.Geteuid()) {
		return -1, errors.New("log quarantine has unsafe metadata")
	}
	closeOnError = false
	return fd, nil
}

func randomQuarantineName() (string, error) {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(random[:]), nil
}

func snapshotMatchesStat(expected fileSnapshot, stat unix.Stat_t) bool {
	current := snapshotFromStat(stat)
	return current.identity == expected.identity && current.links == expected.links && current.mode == expected.mode && current.uid == expected.uid && current.gid == expected.gid
}

func compressFileAt(directory *logDirectory, sourceName string, sourceSnapshot fileSnapshot, logger *FileLogger) (returnErr error) {
	source, openedSource, err := directory.openFile(sourceName, unix.O_RDONLY, false)
	if err != nil {
		return err
	}
	defer source.Close()
	if openedSource.identity != sourceSnapshot.identity {
		return errors.New("rotation source changed before compression")
	}

	temporaryName, temporary, temporarySnapshot, err := createGzipTemporary(directory, sourceName, logger)
	if err != nil {
		return err
	}
	temporaryOpen := true
	published := false
	defer func() {
		if temporaryOpen {
			returnErr = errors.Join(returnErr, temporary.Close())
		}
		if !published {
			if cleanupErr := safeUnlinkAt(directory, temporaryName, temporarySnapshot, logger); cleanupErr != nil && !errors.Is(cleanupErr, unix.ENOENT) {
				returnErr = errors.Join(returnErr, fmt.Errorf("remove gzip temporary: %w", cleanupErr))
			}
		}
	}()

	compressed := gzip.NewWriter(temporary)
	if _, err := io.Copy(compressed, source); err != nil {
		_ = compressed.Close()
		return err
	}
	if err := compressed.Close(); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		temporaryOpen = false
		return err
	}
	temporaryOpen = false
	if err := directory.verifyFile(sourceName, sourceSnapshot); err != nil {
		return fmt.Errorf("rotation source changed during compression: %w", err)
	}
	if err := safeRenameAt(directory, temporaryName, sourceName+".gz", temporarySnapshot, logger); err != nil {
		return err
	}
	published = true
	if err := safeUnlinkAt(directory, sourceName, sourceSnapshot, logger); err != nil {
		return err
	}
	return nil
}

func createGzipTemporary(directory *logDirectory, sourceName string, logger *FileLogger) (string, *os.File, fileSnapshot, error) {
	for attempt := 0; attempt < 16; attempt++ {
		var name string
		if logger.gzipTemporaryName != nil {
			name = logger.gzipTemporaryName(sourceName, attempt)
		} else {
			var random [12]byte
			if _, err := rand.Read(random[:]); err != nil {
				return "", nil, fileSnapshot{}, err
			}
			name = "." + sourceName + ".gzip-" + hex.EncodeToString(random[:])
		}
		file, snapshot, err := directory.createFileExclusive(name, unix.O_WRONLY)
		if err == nil {
			return name, file, snapshot, nil
		}
		if !errors.Is(err, unix.EEXIST) {
			return "", nil, fileSnapshot{}, err
		}
	}
	return "", nil, fileSnapshot{}, errors.New("create unique gzip temporary")
}

func enforceRetention(directory *logDirectory, activeName string, options RotationOptions, now time.Time, logger *FileLogger) error {
	rotations, err := listRotations(directory, activeName)
	if err != nil {
		return fmt.Errorf("enumerate rotated logs for retention: %w", err)
	}
	retained := make([]rotationFile, 0, len(rotations))
	for _, rotation := range rotations {
		remove := options.MaxFiles <= 0 || rotation.suffix > options.MaxFiles
		if !remove && options.MaxAge > 0 {
			remove = now.Sub(time.Unix(rotation.snapshot.modTime, 0)) > options.MaxAge
		}
		if remove {
			if err := safeUnlinkAt(directory, rotation.name, rotation.snapshot, logger); err != nil {
				return fmt.Errorf("remove rotated log: %w", err)
			}
			continue
		}
		retained = append(retained, rotation)
	}
	if options.MaxFiles <= 0 || len(retained) <= options.MaxFiles {
		return nil
	}
	sort.Slice(retained, func(i, j int) bool {
		if retained[i].suffix != retained[j].suffix {
			return retained[i].suffix < retained[j].suffix
		}
		if retained[i].snapshot.modTime != retained[j].snapshot.modTime {
			return retained[i].snapshot.modTime > retained[j].snapshot.modTime
		}
		return retained[i].name < retained[j].name
	})
	for _, rotation := range retained[options.MaxFiles:] {
		if err := safeUnlinkAt(directory, rotation.name, rotation.snapshot, logger); err != nil {
			return fmt.Errorf("remove rotated log over count limit: %w", err)
		}
	}
	return nil
}

func rotationSuffix(name, prefix string) (int, bool, bool) {
	if !strings.HasPrefix(name, prefix) {
		return 0, false, false
	}
	suffix := strings.TrimPrefix(name, prefix)
	compressed := strings.HasSuffix(suffix, ".gz")
	if compressed {
		suffix = strings.TrimSuffix(suffix, ".gz")
	}
	if suffix == "" {
		return 0, false, false
	}
	number, err := strconv.Atoi(suffix)
	if err != nil || number < 1 {
		return 0, false, false
	}
	return number, compressed, true
}

func rotatedName(activeName string, suffix int) string {
	return activeName + "." + strconv.Itoa(suffix)
}

func appendLineAt(directory *logDirectory, name string, line []byte, logger *FileLogger, write func(*os.File, []byte) (int, error)) error {
	file, snapshot, err := directory.openFile(name, unix.O_WRONLY|unix.O_APPEND, true)
	if err != nil {
		return fmt.Errorf("open active log: %w", err)
	}
	if write == nil {
		write = (*os.File).Write
	}
	if logger.beforeMutation != nil {
		logger.beforeMutation("append_recheck", name)
	}
	if err := directory.verifyFile(name, snapshot); err != nil {
		_ = file.Close()
		return fmt.Errorf("active log changed before append: %w", err)
	}
	written, writeErr := write(file, line)
	if written != len(line) {
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		if err := file.Truncate(snapshot.size); err != nil {
			_ = file.Close()
			return fmt.Errorf("append active log: %w", errors.Join(writeErr, fmt.Errorf("rollback partial append: %w", err)))
		}
	}
	if pathErr := directory.verifyFile(name, snapshot); pathErr != nil {
		rollbackErr := file.Truncate(snapshot.size)
		closeErr := file.Close()
		return fmt.Errorf("active log changed during append: %w", errors.Join(pathErr, rollbackErr, closeErr))
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

// Compatibility helpers keep the partial-write rollback unit test focused on
// the real secure append primitive.
func appendLine(path string, line []byte, _ os.FileMode) error {
	return appendLineWithWriter(path, line, 0, nil)
}

func appendLineWithWriter(path string, line []byte, _ os.FileMode, write func(*os.File, []byte) (int, error)) error {
	directory, err := ensureLogDirectory(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.close()
	logger := &FileLogger{directoryIdentity: directory.identity}
	return appendLineAt(directory, filepath.Base(path), line, logger, write)
}
