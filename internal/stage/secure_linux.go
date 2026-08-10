package stage

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const stagedFileName = "backup.sql.gz"

type workspace struct {
	parentPath string
	workName   string
	parentFD   int
	workFD     int
	parentStat unix.Stat_t
	workStat   unix.Stat_t
}

func newWorkspace(parentPath string) (*workspace, error) {
	if !filepath.IsAbs(parentPath) || filepath.Clean(parentPath) != parentPath {
		return nil, errors.New("staging parent must be a clean absolute path")
	}
	parentFD, parentStat, err := openOrCreateSecureDirectory(parentPath)
	if err != nil {
		return nil, err
	}
	workspace := &workspace{parentPath: parentPath, parentFD: parentFD, workFD: -1, parentStat: parentStat}
	if err := verifyPathMatchesDescriptor(parentPath, workspace.parentStat); err != nil {
		workspace.close()
		return nil, err
	}

	workName, err := mkdirRandomAt(parentFD)
	if err != nil {
		workspace.close()
		return nil, err
	}
	workspace.workName = workName
	workFD, err := unix.Openat(parentFD, workName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = unix.Unlinkat(parentFD, workName, unix.AT_REMOVEDIR)
		workspace.close()
		return nil, fmt.Errorf("open private staging directory: %w", err)
	}
	workspace.workFD = workFD
	if err := unix.Fstat(workFD, &workspace.workStat); err != nil {
		workspace.closeAndRemoveDirectory()
		return nil, fmt.Errorf("inspect private staging directory: %w", err)
	}
	if workspace.workStat.Mode&unix.S_IFMT != unix.S_IFDIR || workspace.workStat.Mode&0o777 != 0o700 || int(workspace.workStat.Uid) != os.Geteuid() {
		workspace.closeAndRemoveDirectory()
		return nil, errors.New("private staging directory has unsafe identity or permissions")
	}
	if err := verifyAtMatchesDescriptor(parentFD, workName, workspace.workStat); err != nil {
		workspace.closeAndRemoveDirectory()
		return nil, err
	}
	return workspace, nil
}

func validateRuntimeParent(stat unix.Stat_t) error {
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("staging parent is not a directory")
	}
	euid := os.Geteuid()
	if stat.Uid != 0 && int(stat.Uid) != euid {
		return errors.New("staging parent has an unrelated owner")
	}
	if stat.Mode&0o022 != 0 && stat.Mode&unix.S_ISVTX == 0 {
		return errors.New("shared writable staging parent must have the sticky bit")
	}
	return nil
}

func openOrCreateSecureDirectory(path string) (int, unix.Stat_t, error) {
	currentFD, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, unix.Stat_t{}, fmt.Errorf("open staging path root: %w", err)
	}
	var currentStat unix.Stat_t
	if err := unix.Fstat(currentFD, &currentStat); err != nil {
		_ = unix.Close(currentFD)
		return -1, unix.Stat_t{}, fmt.Errorf("inspect staging path root: %w", err)
	}
	if err := validateRuntimeParent(currentStat); err != nil {
		_ = unix.Close(currentFD)
		return -1, unix.Stat_t{}, fmt.Errorf("unsafe staging path root: %w", err)
	}

	components := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	for _, component := range components {
		if component == "" {
			continue
		}
		nextFD, openErr := unix.Openat(currentFD, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(openErr, unix.ENOENT) {
			mkdirErr := unix.Mkdirat(currentFD, component, 0o700)
			if mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				_ = unix.Close(currentFD)
				return -1, unix.Stat_t{}, fmt.Errorf("create staging path component: %w", mkdirErr)
			}
			nextFD, openErr = unix.Openat(currentFD, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		if openErr != nil {
			_ = unix.Close(currentFD)
			return -1, unix.Stat_t{}, fmt.Errorf("open staging path component without following links: %w", openErr)
		}
		var nextStat unix.Stat_t
		if err := unix.Fstat(nextFD, &nextStat); err != nil {
			_ = unix.Close(nextFD)
			_ = unix.Close(currentFD)
			return -1, unix.Stat_t{}, fmt.Errorf("inspect staging path component: %w", err)
		}
		if err := validateRuntimeParent(nextStat); err != nil {
			_ = unix.Close(nextFD)
			_ = unix.Close(currentFD)
			return -1, unix.Stat_t{}, fmt.Errorf("unsafe staging path component: %w", err)
		}
		if err := verifyAtMatchesDescriptor(currentFD, component, nextStat); err != nil {
			_ = unix.Close(nextFD)
			_ = unix.Close(currentFD)
			return -1, unix.Stat_t{}, err
		}
		if err := unix.Close(currentFD); err != nil {
			_ = unix.Close(nextFD)
			return -1, unix.Stat_t{}, fmt.Errorf("close staging path component: %w", err)
		}
		currentFD = nextFD
		currentStat = nextStat
	}
	return currentFD, currentStat, nil
}

func mkdirRandomAt(parentFD int) (string, error) {
	for attempt := 0; attempt < 16; attempt++ {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", fmt.Errorf("generate private staging directory name: %w", err)
		}
		name := "ezdbbackup-" + hex.EncodeToString(random[:])
		err := unix.Mkdirat(parentFD, name, 0o700)
		if err == nil {
			return name, nil
		}
		if !errors.Is(err, unix.EEXIST) {
			return "", fmt.Errorf("create private staging directory: %w", err)
		}
	}
	return "", errors.New("create unique private staging directory")
}

func (w *workspace) createFile() (result *os.File, stat unix.Stat_t, err error) {
	fd, err := unix.Openat(w.workFD, stagedFileName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, unix.Stat_t{}, fmt.Errorf("create private staging file: %w", err)
	}
	var file *os.File
	removeOnFailure := true
	defer func() {
		if !removeOnFailure {
			return
		}
		if file != nil {
			if closeErr := file.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("close failed staging file: %w", closeErr))
			}
		} else if closeErr := unix.Close(fd); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close failed staging descriptor: %w", closeErr))
		}
		if unlinkErr := unix.Unlinkat(w.workFD, stagedFileName, 0); unlinkErr != nil {
			err = errors.Join(err, fmt.Errorf("remove failed staging file: %w", unlinkErr))
		}
	}()

	file = os.NewFile(uintptr(fd), filepath.Join(w.parentPath, w.workName, stagedFileName))
	if file == nil {
		return nil, unix.Stat_t{}, errors.New("create staging file descriptor")
	}
	if err := unix.Fchmod(fd, 0o600); err != nil {
		return nil, unix.Stat_t{}, fmt.Errorf("set private staging file permissions: %w", err)
	}
	stat, err = statFileDescriptor(file)
	if err != nil {
		return nil, unix.Stat_t{}, fmt.Errorf("inspect private staging file: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o777 != 0o600 || stat.Nlink != 1 || int(stat.Uid) != os.Geteuid() {
		return nil, unix.Stat_t{}, errors.New("private staging file has unsafe identity or permissions")
	}
	removeOnFailure = false
	return file, stat, nil
}

func statFileDescriptor(file *os.File) (unix.Stat_t, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return unix.Stat_t{}, err
	}
	return stat, nil
}

func (w *workspace) artifact(stat unix.Stat_t) Artifact {
	return Artifact{
		Path:         filepath.Join(w.parentPath, w.workName, stagedFileName),
		Size:         stat.Size,
		parentPath:   w.parentPath,
		workName:     w.workName,
		fileName:     stagedFileName,
		parentDevice: uint64(w.parentStat.Dev),
		parentInode:  w.parentStat.Ino,
		workDevice:   uint64(w.workStat.Dev),
		workInode:    w.workStat.Ino,
		device:       uint64(stat.Dev),
		inode:        stat.Ino,
		links:        uint64(stat.Nlink),
		mode:         stat.Mode,
	}
}

func (w *workspace) verifyFinal(artifact Artifact) error {
	if err := verifyAtMatchesDescriptor(w.parentFD, w.workName, w.workStat); err != nil {
		return err
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(w.workFD, stagedFileName, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("verify staged artifact path: %w", err)
	}
	if !artifact.matchesFile(stat, true) {
		return errors.New("staged artifact path identity changed during finalization")
	}
	return nil
}

func (w *workspace) removeArtifact(artifact Artifact, exactSize bool) error {
	var stat unix.Stat_t
	if err := unix.Fstatat(w.workFD, stagedFileName, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("inspect partial staging artifact: %w", err)
	}
	if !artifact.matchesFile(stat, exactSize) {
		return errors.New("partial staging artifact identity changed; refusing removal")
	}
	if err := unix.Unlinkat(w.workFD, stagedFileName, 0); err != nil {
		return fmt.Errorf("remove partial staging artifact: %w", err)
	}
	return w.removeDirectory()
}

func (w *workspace) removeDirectory() error {
	if w.workName == "" {
		return nil
	}
	if w.workFD >= 0 {
		if err := verifyAtMatchesDescriptor(w.parentFD, w.workName, w.workStat); err != nil {
			return err
		}
	}
	if err := unix.Unlinkat(w.parentFD, w.workName, unix.AT_REMOVEDIR); err != nil {
		return fmt.Errorf("remove private staging directory: %w", err)
	}
	w.workName = ""
	return nil
}

func (w *workspace) closeAndRemoveDirectory() {
	if w.workFD >= 0 {
		_ = unix.Close(w.workFD)
		w.workFD = -1
	}
	if w.workName != "" && w.parentFD >= 0 {
		_ = unix.Unlinkat(w.parentFD, w.workName, unix.AT_REMOVEDIR)
	}
	w.close()
}

func (w *workspace) close() {
	if w.workFD >= 0 {
		_ = unix.Close(w.workFD)
		w.workFD = -1
	}
	if w.parentFD >= 0 {
		_ = unix.Close(w.parentFD)
		w.parentFD = -1
	}
}

func openVerifiedArtifact(artifact Artifact) (*os.File, error) {
	parentFD, workFD, err := openVerifiedDirectories(artifact)
	if err != nil {
		return nil, err
	}
	defer unix.Close(parentFD)
	defer unix.Close(workFD)

	fd, err := unix.Openat(workFD, artifact.fileName, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open staged artifact without following links: %w", err)
	}
	var descriptorStat unix.Stat_t
	if err := unix.Fstat(fd, &descriptorStat); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("inspect staged artifact descriptor: %w", err)
	}
	if !artifact.matchesFile(descriptorStat, true) {
		_ = unix.Close(fd)
		return nil, errors.New("staged artifact descriptor identity changed")
	}
	var pathStat unix.Stat_t
	if err := unix.Fstatat(workFD, artifact.fileName, &pathStat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("verify staged artifact path after open: %w", err)
	}
	if !sameStatObject(descriptorStat, pathStat) || !artifact.matchesFile(pathStat, true) {
		_ = unix.Close(fd)
		return nil, errors.New("staged artifact path changed after open")
	}
	file := os.NewFile(uintptr(fd), artifact.Path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("create verified staging file descriptor")
	}
	return file, nil
}

func removeVerifiedArtifact(artifact Artifact) error {
	parentFD, workFD, err := openVerifiedDirectories(artifact)
	if err != nil {
		return err
	}
	defer unix.Close(parentFD)

	var stat unix.Stat_t
	if err := unix.Fstatat(workFD, artifact.fileName, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		_ = unix.Close(workFD)
		return fmt.Errorf("inspect staged artifact before cleanup: %w", err)
	}
	if !artifact.matchesFile(stat, true) {
		_ = unix.Close(workFD)
		return errors.New("staged artifact identity changed; refusing cleanup")
	}
	if err := unix.Unlinkat(workFD, artifact.fileName, 0); err != nil {
		_ = unix.Close(workFD)
		return fmt.Errorf("remove staged artifact: %w", err)
	}
	if err := verifyDirectoryAt(parentFD, artifact.workName, artifact.workDevice, artifact.workInode); err != nil {
		_ = unix.Close(workFD)
		return err
	}
	if err := unix.Close(workFD); err != nil {
		return fmt.Errorf("close private staging directory: %w", err)
	}
	if err := unix.Unlinkat(parentFD, artifact.workName, unix.AT_REMOVEDIR); err != nil {
		return fmt.Errorf("remove private staging directory: %w", err)
	}
	return nil
}

func openVerifiedDirectories(artifact Artifact) (int, int, error) {
	if !artifact.validShape() {
		return -1, -1, errors.New("staging artifact identity is incomplete")
	}
	parentFD, err := unix.Open(artifact.parentPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, -1, fmt.Errorf("open staging parent for identity check: %w", err)
	}
	var parentStat unix.Stat_t
	if err := unix.Fstat(parentFD, &parentStat); err != nil {
		_ = unix.Close(parentFD)
		return -1, -1, fmt.Errorf("inspect staging parent descriptor: %w", err)
	}
	if uint64(parentStat.Dev) != artifact.parentDevice || parentStat.Ino != artifact.parentInode || parentStat.Mode&unix.S_IFMT != unix.S_IFDIR {
		_ = unix.Close(parentFD)
		return -1, -1, errors.New("staging parent identity changed")
	}
	if err := verifyPathMatchesDescriptor(artifact.parentPath, parentStat); err != nil {
		_ = unix.Close(parentFD)
		return -1, -1, err
	}
	workFD, err := unix.Openat(parentFD, artifact.workName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = unix.Close(parentFD)
		return -1, -1, fmt.Errorf("open private staging directory for identity check: %w", err)
	}
	var workStat unix.Stat_t
	if err := unix.Fstat(workFD, &workStat); err != nil {
		_ = unix.Close(workFD)
		_ = unix.Close(parentFD)
		return -1, -1, fmt.Errorf("inspect private staging directory descriptor: %w", err)
	}
	if uint64(workStat.Dev) != artifact.workDevice || workStat.Ino != artifact.workInode || workStat.Mode&unix.S_IFMT != unix.S_IFDIR || workStat.Mode&0o777 != 0o700 {
		_ = unix.Close(workFD)
		_ = unix.Close(parentFD)
		return -1, -1, errors.New("private staging directory identity changed")
	}
	if err := verifyAtMatchesDescriptor(parentFD, artifact.workName, workStat); err != nil {
		_ = unix.Close(workFD)
		_ = unix.Close(parentFD)
		return -1, -1, err
	}
	return parentFD, workFD, nil
}

func (a Artifact) validShape() bool {
	return filepath.IsAbs(a.parentPath) && filepath.Clean(a.parentPath) == a.parentPath &&
		a.workName != "" && filepath.Base(a.workName) == a.workName &&
		a.fileName == stagedFileName && filepath.Base(a.fileName) == a.fileName &&
		a.Path == filepath.Join(a.parentPath, a.workName, a.fileName) && a.Size >= 0 && a.links == 1
}

func (a Artifact) matchesFile(stat unix.Stat_t, exactSize bool) bool {
	expectedMode := uint32(unix.S_IFREG | 0o600)
	if uint64(stat.Dev) != a.device || stat.Ino != a.inode || uint64(stat.Nlink) != a.links ||
		a.mode != expectedMode || stat.Mode != a.mode {
		return false
	}
	return !exactSize || stat.Size == a.Size
}

func verifyPathMatchesDescriptor(path string, descriptor unix.Stat_t) error {
	var pathStat unix.Stat_t
	if err := unix.Lstat(path, &pathStat); err != nil {
		return fmt.Errorf("verify staging path identity: %w", err)
	}
	if !sameStatObject(descriptor, pathStat) || pathStat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("staging path identity changed after open")
	}
	return nil
}

func verifyAtMatchesDescriptor(parentFD int, name string, descriptor unix.Stat_t) error {
	var pathStat unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &pathStat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("verify private staging directory identity: %w", err)
	}
	if !sameStatObject(descriptor, pathStat) || pathStat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("private staging directory path identity changed after open")
	}
	return nil
}

func verifyDirectoryAt(parentFD int, name string, device, inode uint64) error {
	var stat unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("verify private staging directory before cleanup: %w", err)
	}
	if uint64(stat.Dev) != device || stat.Ino != inode || stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("private staging directory changed; refusing cleanup")
	}
	return nil
}

func sameStatObject(left, right unix.Stat_t) bool {
	return left.Dev == right.Dev && left.Ino == right.Ino
}
