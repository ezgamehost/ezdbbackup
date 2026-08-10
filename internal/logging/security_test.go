package logging

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestNewRejectsHugeRotationHistoryBeforeFilesystemMutation(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "logs")
	_, err := New(Options{Directory: directory, Rotation: RotationOptions{MaxFiles: math.MaxInt}})
	if err == nil || !strings.Contains(err.Error(), "max_files") {
		t.Fatalf("New(MaxInt) error = %v, want bounded-history rejection", err)
	}
	if _, statErr := os.Lstat(directory); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("New(MaxInt) mutated the filesystem: %v", statErr)
	}
}

func TestNewRejectsUnsafePreexistingLogObjects(t *testing.T) {
	tests := []struct {
		name  string
		plant func(t *testing.T, dir, victim string)
	}{
		{
			name: "active symlink",
			plant: func(t *testing.T, dir, victim string) {
				t.Helper()
				if err := os.Symlink(victim, filepath.Join(dir, "info.log")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "lock symlink",
			plant: func(t *testing.T, dir, victim string) {
				t.Helper()
				if err := os.Symlink(victim, filepath.Join(dir, "info.log.lock")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "active hard link",
			plant: func(t *testing.T, dir, victim string) {
				t.Helper()
				if err := os.Link(victim, filepath.Join(dir, "info.log")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "lock hard link",
			plant: func(t *testing.T, dir, victim string) {
				t.Helper()
				if err := os.Link(victim, filepath.Join(dir, "info.log.lock")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "rotated symlink",
			plant: func(t *testing.T, dir, victim string) {
				t.Helper()
				if err := os.Symlink(victim, filepath.Join(dir, "info.log.7")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "rotated hard link",
			plant: func(t *testing.T, dir, victim string) {
				t.Helper()
				if err := os.Link(victim, filepath.Join(dir, "info.log.7")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "unsafe active mode",
			plant: func(t *testing.T, dir, _ string) {
				t.Helper()
				writeLogFixture(t, filepath.Join(dir, "info.log"), "unsafe", 0o666)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := secureLogDir(t)
			victim := filepath.Join(t.TempDir(), "victim")
			const original = "must remain unchanged"
			if err := os.WriteFile(victim, []byte(original), 0o640); err != nil {
				t.Fatal(err)
			}
			tt.plant(t, dir, victim)

			if _, err := New(Options{Directory: dir, Rotation: RotationOptions{MaxFiles: 7}}); err == nil {
				t.Fatal("New() accepted an unsafe preexisting log object")
			}
			got, err := os.ReadFile(victim)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != original {
				t.Fatalf("victim = %q, want unchanged", got)
			}
		})
	}
}

func TestNewRejectsUnsafeLogDirectoryModes(t *testing.T) {
	for _, mode := range []os.FileMode{0o770, 0o757, 0o777} {
		t.Run(mode.String(), func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, mode); err != nil {
				t.Fatal(err)
			}
			if _, err := New(Options{Directory: dir}); err == nil || !strings.Contains(err.Error(), "unsafe") {
				t.Fatalf("New() error = %v, want unsafe-directory rejection", err)
			}
		})
	}
}

func TestSharedGroupMembershipIncludesEffectiveAndSupplementaryGroups(t *testing.T) {
	if !groupListContains(2000, 2000, nil) {
		t.Fatal("effective group was not recognized")
	}
	if !groupListContains(2000, 1000, []int{2000}) {
		t.Fatal("supplementary group was not recognized")
	}
	if groupListContains(2000, 1000, []int{3000}) {
		t.Fatal("unrelated group was accepted")
	}
}

func TestPrivateLogPolicyRejectsUnrelatedReadableGroupAndUnsafeFileMetadata(t *testing.T) {
	unrelatedGID := uint32(1 << 30)
	stat := unix.Stat_t{
		Uid:  uint32(os.Geteuid()),
		Gid:  unrelatedGID,
		Mode: unix.S_IFDIR | unix.S_ISGID | 0o750,
	}
	if _, _, err := validateLogDirectory(stat); err == nil || !strings.Contains(err.Error(), "group") {
		t.Fatalf("validateLogDirectory(unrelated readable group) error = %v", err)
	}

	directory := &logDirectory{fileMode: privateLogFileMode}
	fileStat := unix.Stat_t{
		Uid:   uint32(os.Geteuid()),
		Gid:   unrelatedGID,
		Mode:  unix.S_IFREG | privateLogFileMode,
		Nlink: 1,
	}
	if err := directory.validateFile("info.log", fileStat); err == nil || !strings.Contains(err.Error(), "group") {
		t.Fatalf("validateFile(unrelated group) error = %v", err)
	}
	fileStat.Gid = uint32(os.Getegid())
	fileStat.Mode |= unix.S_ISUID
	if err := directory.validateFile("info.log", fileStat); err == nil || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("validateFile(setuid) error = %v, want exact-mode rejection", err)
	}

	shared := &logDirectory{shared: true, fileMode: sharedLogFileMode, gid: uint32(os.Getegid())}
	fileStat.Mode = unix.S_IFREG | sharedLogFileMode
	fileStat.Gid = uint32(os.Getegid())
	fileStat.Uid = uint32(1 << 30)
	if err := shared.validateFile("info.log", fileStat); err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("validateFile(unrelated shared owner) error = %v, want owner rejection", err)
	}
}

func TestNewRotatesAndRetainsEveryInitializedFamily(t *testing.T) {
	dir := secureLogDir(t)
	now := time.Now()
	for _, name := range []string{"error.log", "info.log", "debug.log"} {
		writeLogFixture(t, filepath.Join(dir, name), strings.Repeat("x", 256), 0o640)
		old := filepath.Join(dir, name+".9")
		writeLogFixture(t, old, "old", 0o640)
		if err := os.Chtimes(old, now.Add(-48*time.Hour), now.Add(-48*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := New(Options{
		Directory: dir,
		Debug:     true,
		Rotation:  RotationOptions{MaxSizeBytes: 128, MaxFiles: 2, MaxAge: 24 * time.Hour},
	}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"error.log", "info.log", "debug.log"} {
		if _, err := os.Stat(filepath.Join(dir, name+".1")); err != nil {
			t.Fatalf("%s was not rotated during New: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(dir, name+".9")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale %s generation survived New: %v", name, err)
		}
	}
}

func TestSharedGroupDirectoryCreatesExactWritableModes(t *testing.T) {
	dir := secureLogDir(t)
	if err := os.Chmod(dir, os.ModeSetgid|0o770); err != nil {
		t.Fatal(err)
	}
	logger, err := New(Options{
		Directory: dir,
		Rotation:  RotationOptions{MaxSizeBytes: 1, MaxFiles: 2, Compress: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := logger.Write(Event{Level: InfoLevel, Message: "first", Command: "backup"}); err != nil {
		t.Fatal(err)
	}
	if err := logger.Write(Event{Level: InfoLevel, Message: "second", Command: "backup"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"error.log", "error.log.lock", "info.log", "info.log.lock", "info.log.1.gz"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if got := info.Mode().Perm(); got != 0o660 {
			t.Fatalf("%s mode = %#o, want 0660", name, got)
		}
	}
}

func TestRotationShiftsOnlyEnumeratedSparseGenerations(t *testing.T) {
	dir := secureLogDir(t)
	writeLogFixture(t, filepath.Join(dir, "info.log"), strings.Repeat("x", 256), 0o640)
	writeLogFixture(t, filepath.Join(dir, "info.log.500"), "sparse", 0o640)
	logger, err := New(Options{
		Directory: dir,
		Rotation:  RotationOptions{MaxSizeBytes: 128, MaxFiles: MaxRotationFiles},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "info.log.501")); err != nil {
		t.Fatalf("sparse generation was not shifted: %v", err)
	}
	if got := logger.rotationMutationCount.Load(); got != 2 {
		t.Fatalf("rotation mutations = %d, want active + one existing generation", got)
	}
}

func TestAppendRejectsPathReplacementWithoutTouchingReplacement(t *testing.T) {
	dir := secureLogDir(t)
	logger, err := New(Options{Directory: dir})
	if err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(dir, "info.log")
	original := active + ".original"
	const replacement = "unrelated replacement"
	logger.beforeMutation = func(operation, name string) {
		if operation != "append_recheck" || name != "info.log" {
			return
		}
		logger.beforeMutation = nil
		if err := os.Rename(active, original); err != nil {
			t.Fatal(err)
		}
		writeLogFixture(t, active, replacement, 0o640)
	}

	err = logger.Write(Event{Level: InfoLevel, Message: "must not append", Command: "backup"})
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("Write() error = %v, want identity-change rejection", err)
	}
	got, readErr := os.ReadFile(active)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != replacement {
		t.Fatalf("replacement = %q, want untouched", got)
	}
}

func TestStableLockRejectsReplacementAfterFlock(t *testing.T) {
	dir := secureLogDir(t)
	logger, err := New(Options{Directory: dir})
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(dir, "info.log.lock")
	const replacement = "replacement lock"
	logger.beforeMutation = func(operation, name string) {
		if operation != "lock_recheck" || name != "info.log.lock" {
			return
		}
		logger.beforeMutation = nil
		if err := os.Rename(lockPath, lockPath+".original"); err != nil {
			t.Fatal(err)
		}
		writeLogFixture(t, lockPath, replacement, 0o640)
	}

	err = logger.Write(Event{Level: InfoLevel, Message: "blocked", Command: "backup"})
	if err == nil || !strings.Contains(err.Error(), "lock") {
		t.Fatalf("Write() error = %v, want stable-lock rejection", err)
	}
	got, readErr := os.ReadFile(lockPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != replacement {
		t.Fatalf("replacement lock = %q, want untouched", got)
	}
}

func TestRetentionRefusesToUnlinkReplacement(t *testing.T) {
	dir := secureLogDir(t)
	logger, err := New(Options{Directory: dir, Rotation: RotationOptions{MaxFiles: 2, MaxAge: time.Hour}})
	if err != nil {
		t.Fatal(err)
	}
	rotation := filepath.Join(dir, "info.log.1")
	writeLogFixture(t, rotation, "old", 0o640)
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(rotation, old, old); err != nil {
		t.Fatal(err)
	}
	logger.beforeMutation = func(operation, name string) {
		if operation != "remove_recheck" || name != "info.log.1" {
			return
		}
		logger.beforeMutation = nil
		if err := os.Rename(rotation, rotation+".original"); err != nil {
			t.Fatal(err)
		}
		writeLogFixture(t, rotation, "replacement", 0o640)
	}

	err = logger.Write(Event{Level: InfoLevel, Message: "retention", Command: "backup"})
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("Write() error = %v, want replacement rejection", err)
	}
	got, readErr := os.ReadFile(rotation)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "replacement" {
		t.Fatalf("replacement rotation = %q, want untouched", got)
	}
}

func TestRetentionQuarantinesAndRestoresReplacementSwappedAfterVerification(t *testing.T) {
	dir := secureLogDir(t)
	logger, err := New(Options{Directory: dir, Rotation: RotationOptions{MaxFiles: 2, MaxAge: time.Hour}})
	if err != nil {
		t.Fatal(err)
	}
	rotation := filepath.Join(dir, "info.log.1")
	original := rotation + ".original"
	writeLogFixture(t, rotation, "old", 0o640)
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(rotation, old, old); err != nil {
		t.Fatal(err)
	}
	logger.beforeMutation = func(operation, name string) {
		if operation != "remove_after_verify" || name != "info.log.1" {
			return
		}
		logger.beforeMutation = nil
		if err := os.Rename(rotation, original); err != nil {
			t.Fatal(err)
		}
		writeLogFixture(t, rotation, "replacement", 0o640)
	}

	err = logger.Write(Event{Level: InfoLevel, Message: "retention", Command: "backup"})
	if err == nil || !strings.Contains(err.Error(), "after verification") {
		t.Fatalf("Write() error = %v, want post-verification identity rejection", err)
	}
	for path, want := range map[string]string{rotation: "replacement", original: "old"} {
		got, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(got) != want {
			t.Fatalf("%s = %q, want %q", path, got, want)
		}
	}
}

func TestPinnedDirectoryLockSerializesAfterVisibleLockReplacement(t *testing.T) {
	dir := secureLogDir(t)
	if _, err := New(Options{Directory: dir}); err != nil {
		t.Fatal(err)
	}
	firstDirectory, err := ensureLogDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer firstDirectory.close()
	secondDirectory, err := ensureLogDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer secondDirectory.close()

	entered := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	logger := &FileLogger{}
	go func() {
		firstDone <- logger.withFamilyLock(firstDirectory, "info.log", false, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	lockPath := filepath.Join(dir, "info.log.lock")
	if err := os.Rename(lockPath, lockPath+".original"); err != nil {
		t.Fatal(err)
	}
	writeLogFixture(t, lockPath, "replacement", 0o640)
	if err := unix.Flock(secondDirectory.fd, unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
		if err == nil {
			_ = unix.Flock(secondDirectory.fd, unix.LOCK_UN)
		}
		t.Fatalf("second directory lock error = %v, want contention", err)
	}
	close(release)
	if err := <-firstDone; err == nil || !strings.Contains(err.Error(), "during operation") {
		t.Fatalf("first lock error = %v, want replacement detection", err)
	}
}

func TestRotationNeverOverwritesRacingDestination(t *testing.T) {
	dir := secureLogDir(t)
	logger, err := New(Options{Directory: dir, Rotation: RotationOptions{MaxSizeBytes: 1, MaxFiles: 3}})
	if err != nil {
		t.Fatal(err)
	}
	if err := logger.Write(Event{Level: InfoLevel, Message: "first", Command: "backup"}); err != nil {
		t.Fatal(err)
	}
	if err := logger.Write(Event{Level: InfoLevel, Message: "second", Command: "backup"}); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(dir, "info.log.2")
	logger.beforeMutation = func(operation, name string) {
		if operation != "rename_recheck" || name != "info.log.1" {
			return
		}
		logger.beforeMutation = nil
		writeLogFixture(t, destination, "unrelated destination", 0o640)
	}

	err = logger.Write(Event{Level: InfoLevel, Message: "third", Command: "backup"})
	if err == nil {
		t.Fatal("Write() overwrote a racing rotation destination")
	}
	got, readErr := os.ReadFile(destination)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "unrelated destination" {
		t.Fatalf("rotation destination = %q, want untouched", got)
	}
}

func TestCompressionTemporaryNeverFollowsPreexistingEntry(t *testing.T) {
	dir := secureLogDir(t)
	logger, err := New(Options{Directory: dir, Rotation: RotationOptions{MaxSizeBytes: 1, MaxFiles: 2, Compress: true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := logger.Write(Event{Level: InfoLevel, Message: "first", Command: "backup"}); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "victim")
	writeLogFixture(t, victim, "victim", 0o640)
	temporaryName := ".info.log.1.gzip-fixed"
	if err := os.Symlink(victim, filepath.Join(dir, temporaryName)); err != nil {
		t.Fatal(err)
	}
	logger.gzipTemporaryName = func(string, int) string { return temporaryName }

	err = logger.Write(Event{Level: InfoLevel, Message: "second", Command: "backup"})
	if err == nil {
		t.Fatal("Write() accepted a preexisting gzip temporary")
	}
	got, readErr := os.ReadFile(victim)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "victim" {
		t.Fatalf("gzip temporary victim = %q, want untouched", got)
	}
}

func TestConfiguredDirectorySymlinkIsBoundToOriginalTarget(t *testing.T) {
	root := secureLogDir(t)
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	for _, path := range []string{first, second} {
		if err := os.Mkdir(path, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "current")
	if err := os.Symlink(first, link); err != nil {
		t.Fatal(err)
	}
	logger, err := New(Options{Directory: link})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, link); err != nil {
		t.Fatal(err)
	}

	err = logger.Write(Event{Level: InfoLevel, Message: "must not move", Command: "backup"})
	if err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("Write() error = %v, want directory identity rejection", err)
	}
	data, readErr := os.ReadFile(filepath.Join(second, "info.log"))
	if !errors.Is(readErr, os.ErrNotExist) || len(data) != 0 {
		t.Fatalf("replacement directory received log data %q, error %v", data, readErr)
	}
}

func TestDirectoryWalkRejectsComponentReplacementAfterOpen(t *testing.T) {
	root := secureLogDir(t)
	leaf := filepath.Join(root, "leaf")
	if err := os.Mkdir(leaf, 0o750); err != nil {
		t.Fatal(err)
	}
	replacement := leaf + ".replacement"
	_, _, err := openDirectoryDescriptorWithHook(leaf, func(_ int, name string) {
		if name != "leaf" {
			return
		}
		if renameErr := os.Rename(leaf, leaf+".original"); renameErr != nil {
			t.Fatal(renameErr)
		}
		if mkdirErr := os.Mkdir(replacement, 0o750); mkdirErr != nil {
			t.Fatal(mkdirErr)
		}
		if renameErr := os.Rename(replacement, leaf); renameErr != nil {
			t.Fatal(renameErr)
		}
	})
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("openDirectoryDescriptorWithHook(swapped component) error = %v, want identity rejection", err)
	}
	if info, statErr := os.Stat(leaf); statErr != nil || !info.IsDir() {
		t.Fatalf("replacement directory was altered: info=%v error=%v", info, statErr)
	}
}

func TestExistingLoggerReopensActiveFileAfterAnotherLoggerRotates(t *testing.T) {
	dir := secureLogDir(t)
	options := Options{Directory: dir, Rotation: RotationOptions{MaxSizeBytes: 1, MaxFiles: 3}}
	first, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Write(Event{Level: InfoLevel, Message: "before", Command: "backup"}); err != nil {
		t.Fatal(err)
	}
	if err := second.Write(Event{Level: InfoLevel, Message: "rotator", Command: "backup"}); err != nil {
		t.Fatal(err)
	}
	if err := first.Write(Event{Level: InfoLevel, Message: "after", Command: "backup"}); err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	for _, name := range []string{"info.log", "info.log.1", "info.log.2", "info.log.3"} {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		for _, line := range readLogLines(t, path) {
			seen[line["message"].(string)] = true
		}
	}
	for _, message := range []string{"before", "rotator", "after"} {
		if !seen[message] {
			t.Fatalf("message %q missing after cross-logger rotations: %v", message, seen)
		}
	}
}

func writeLogFixture(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func secureLogDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	return dir
}
