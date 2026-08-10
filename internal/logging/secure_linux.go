package logging

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	privateLogFileMode uint32      = 0o640
	sharedLogFileMode  uint32      = 0o660
	logFileMode        os.FileMode = 0o640
)

type logIdentity struct {
	device uint64
	inode  uint64
}

type fileSnapshot struct {
	identity logIdentity
	mode     uint32
	uid      uint32
	gid      uint32
	links    uint64
	size     int64
	modTime  int64
}

type logDirectory struct {
	fd         int
	configured string
	resolved   string
	identity   logIdentity
	uid        uint32
	gid        uint32
	shared     bool
	fileMode   uint32
}

// DirectoryBinding carries a read-only descriptor snapshot from the final
// pre-initialization check into New. Its fields are intentionally private so
// callers cannot forge a trusted directory identity.
type DirectoryBinding struct {
	configured string
	resolved   string
	identity   logIdentity
}

// BindDirectory snapshots an existing configured log directory immediately
// before logger initialization. A missing directory returns a nil binding;
// New then creates it through the already descriptor-relative path walk.
func BindDirectory(path string) (*DirectoryBinding, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("log directory must be a clean absolute path")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := validateExistingLogSymlinkOwners(path); err != nil {
			return nil, fmt.Errorf("unsafe existing lexical log symlink: %w", err)
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("resolve log directory binding: %w", err)
	}
	directory, err := openLogDirectory(path, resolved, nil)
	if err != nil {
		return nil, err
	}
	binding := &DirectoryBinding{configured: path, resolved: resolved, identity: directory.identity}
	if err := directory.close(); err != nil {
		return nil, fmt.Errorf("close bound log directory: %w", err)
	}
	return binding, nil
}

func ensureBoundLogDirectory(path string, binding *DirectoryBinding) (*logDirectory, error) {
	if binding == nil {
		return ensureLogDirectory(path)
	}
	if binding.configured != path {
		return nil, errors.New("log directory binding does not match configured path")
	}
	return openLogDirectory(path, binding.resolved, &binding.identity)
}

func ensureLogDirectory(path string) (*logDirectory, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("log directory must be a clean absolute path")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return openLogDirectory(path, resolved, nil)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("resolve log directory: %w", err)
	}

	return createMissingLogDirectory(path)
}

// createMissingLogDirectory walks the lexical path from the filesystem root
// without following any symlink. Existing configured directories may use
// trusted symlinks, but creation must never mutate a target selected during a
// racy pathname resolution.
func createMissingLogDirectory(path string) (*logDirectory, error) {
	return createMissingLogDirectoryWithHook(path, nil)
}

func createMissingLogDirectoryWithHook(path string, beforeComponentOpen func(string)) (*logDirectory, error) {
	fd, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open log path root: %w", err)
	}
	var currentStat unix.Stat_t
	if err := unix.Fstat(fd, &currentStat); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("inspect log path root: %w", err)
	}
	if err := validateLogPathParent(currentStat); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("unsafe log path root: %w", err)
	}
	components := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	remaining := 0
	for _, component := range components {
		if component != "" {
			remaining++
		}
	}
	for _, component := range components {
		if component == "" {
			continue
		}
		remaining--
		created := false
		if beforeComponentOpen != nil {
			beforeComponentOpen(component)
		}
		nextFD, openErr := unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(openErr, unix.ENOENT) {
			mkdirErr := unix.Mkdirat(fd, component, logDirectoryMode)
			if mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				_ = unix.Close(fd)
				return nil, fmt.Errorf("create log directory component %q: %w", component, mkdirErr)
			}
			created = mkdirErr == nil
			nextFD, openErr = unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		if openErr != nil {
			_ = unix.Close(fd)
			return nil, fmt.Errorf("open log directory component %q without following links: %w", component, openErr)
		}
		if created {
			if err := unix.Fchmod(nextFD, logDirectoryMode); err != nil {
				_ = unix.Close(nextFD)
				_ = unix.Close(fd)
				return nil, fmt.Errorf("set new log directory permissions: %w", err)
			}
		}
		var nextStat unix.Stat_t
		if err := unix.Fstat(nextFD, &nextStat); err != nil {
			_ = unix.Close(nextFD)
			_ = unix.Close(fd)
			return nil, fmt.Errorf("inspect log directory component: %w", err)
		}
		var entryStat unix.Stat_t
		if err := unix.Fstatat(fd, component, &entryStat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			_ = unix.Close(nextFD)
			_ = unix.Close(fd)
			return nil, fmt.Errorf("recheck log directory component: %w", err)
		}
		if !sameStatIdentity(nextStat, entryStat) || entryStat.Mode&unix.S_IFMT != unix.S_IFDIR {
			_ = unix.Close(nextFD)
			_ = unix.Close(fd)
			return nil, errors.New("log directory component changed while opening")
		}
		if remaining > 0 {
			if err := validateLogPathParent(nextStat); err != nil {
				_ = unix.Close(nextFD)
				_ = unix.Close(fd)
				return nil, fmt.Errorf("unsafe log directory parent component: %w", err)
			}
		}
		if err := unix.Close(fd); err != nil {
			_ = unix.Close(nextFD)
			return nil, fmt.Errorf("close log directory ancestor: %w", err)
		}
		fd = nextFD
		currentStat = nextStat
	}
	shared, fileMode, err := validateLogDirectory(currentStat)
	if err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("unsafe log directory: %w", err)
	}
	return &logDirectory{
		fd:         fd,
		configured: path,
		resolved:   path,
		identity:   identityFromStat(currentStat),
		uid:        currentStat.Uid,
		gid:        currentStat.Gid,
		shared:     shared,
		fileMode:   fileMode,
	}, nil
}

func reopenLogDirectory(path string, expected logIdentity) (*logDirectory, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("resolve log directory: %w", err)
	}
	return openLogDirectory(path, resolved, &expected)
}

func openLogDirectory(configured, resolved string, expected *logIdentity) (*logDirectory, error) {
	if err := validateLogSymlinkOwners(configured); err != nil {
		return nil, fmt.Errorf("unsafe lexical log symlink: %w", err)
	}
	if err := validateLogPathParents(configured); err != nil {
		return nil, fmt.Errorf("unsafe lexical log path: %w", err)
	}
	fd, stat, err := openDirectoryDescriptor(resolved)
	if err != nil {
		return nil, err
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = unix.Close(fd)
		}
	}()
	identity := identityFromStat(stat)
	currentResolved, err := filepath.EvalSymlinks(configured)
	if err != nil {
		return nil, fmt.Errorf("re-resolve log directory: %w", err)
	}
	if currentResolved != resolved {
		return nil, errors.New("configured log directory changed while opening")
	}
	if err := validateLogSymlinkOwners(configured); err != nil {
		return nil, fmt.Errorf("unsafe lexical log symlink after open: %w", err)
	}
	if err := validateLogPathParents(configured); err != nil {
		return nil, fmt.Errorf("unsafe lexical log path after open: %w", err)
	}
	if expected != nil && identity != *expected {
		return nil, errors.New("log directory identity changed")
	}
	shared, fileMode, err := validateLogDirectory(stat)
	if err != nil {
		return nil, fmt.Errorf("unsafe log directory: %w", err)
	}
	closeOnError = false
	return &logDirectory{
		fd:         fd,
		configured: configured,
		resolved:   resolved,
		identity:   identity,
		uid:        stat.Uid,
		gid:        stat.Gid,
		shared:     shared,
		fileMode:   fileMode,
	}, nil
}

func validateLogSymlinkOwners(path string) error {
	return validateLogSymlinkOwnersUntilMissing(path, false)
}

func validateExistingLogSymlinkOwners(path string) error {
	return validateLogSymlinkOwnersUntilMissing(path, true)
}

func validateLogSymlinkOwnersUntilMissing(path string, allowMissing bool) error {
	current := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(filepath.Clean(path), current), current) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		var stat unix.Stat_t
		if err := unix.Lstat(current, &stat); err != nil {
			if allowMissing && errors.Is(err, unix.ENOENT) {
				return nil
			}
			return fmt.Errorf("inspect path component %q: %w", current, err)
		}
		if stat.Mode&unix.S_IFMT == unix.S_IFLNK && stat.Uid != 0 && stat.Uid != uint32(os.Geteuid()) {
			return fmt.Errorf("symbolic link %q has an unrelated owner", current)
		}
	}
	return nil
}

func openDirectoryDescriptor(path string) (int, unix.Stat_t, error) {
	return openDirectoryDescriptorWithHook(path, nil)
}

func openDirectoryDescriptorWithHook(path string, beforeEntryRecheck func(parentFD int, name string)) (int, unix.Stat_t, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return -1, unix.Stat_t{}, errors.New("log directory path must be a clean absolute path")
	}
	fd, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, unix.Stat_t{}, fmt.Errorf("open log path root: %w", err)
	}
	var current unix.Stat_t
	if err := unix.Fstat(fd, &current); err != nil {
		_ = unix.Close(fd)
		return -1, unix.Stat_t{}, fmt.Errorf("inspect log path root: %w", err)
	}
	components := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	remaining := 0
	for _, component := range components {
		if component != "" {
			remaining++
		}
	}
	if remaining > 0 {
		if err := validateLogPathParent(current); err != nil {
			_ = unix.Close(fd)
			return -1, unix.Stat_t{}, fmt.Errorf("unsafe log path root: %w", err)
		}
	}
	for _, component := range components {
		if component == "" {
			continue
		}
		remaining--
		nextFD, openErr := unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr != nil {
			_ = unix.Close(fd)
			return -1, unix.Stat_t{}, fmt.Errorf("open log path component %q without following links: %w", component, openErr)
		}
		var next unix.Stat_t
		if err := unix.Fstat(nextFD, &next); err != nil {
			_ = unix.Close(nextFD)
			_ = unix.Close(fd)
			return -1, unix.Stat_t{}, fmt.Errorf("inspect log path component %q: %w", component, err)
		}
		if beforeEntryRecheck != nil {
			beforeEntryRecheck(fd, component)
		}
		var entry unix.Stat_t
		if err := unix.Fstatat(fd, component, &entry, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			_ = unix.Close(nextFD)
			_ = unix.Close(fd)
			return -1, unix.Stat_t{}, fmt.Errorf("recheck log path component %q: %w", component, err)
		}
		if next.Mode&unix.S_IFMT != unix.S_IFDIR || entry.Mode&unix.S_IFMT != unix.S_IFDIR || !sameStatIdentity(next, entry) {
			_ = unix.Close(nextFD)
			_ = unix.Close(fd)
			return -1, unix.Stat_t{}, fmt.Errorf("log path component %q changed while opening", component)
		}
		if remaining > 0 {
			if err := validateLogPathParent(next); err != nil {
				_ = unix.Close(nextFD)
				_ = unix.Close(fd)
				return -1, unix.Stat_t{}, fmt.Errorf("unsafe log path component %q: %w", component, err)
			}
		}
		if err := unix.Close(fd); err != nil {
			_ = unix.Close(nextFD)
			return -1, unix.Stat_t{}, fmt.Errorf("close log path component: %w", err)
		}
		fd = nextFD
		current = next
	}
	return fd, current, nil
}

func validateLogPathParents(path string) error {
	parent := filepath.Dir(filepath.Clean(path))
	for {
		var stat unix.Stat_t
		if err := unix.Stat(parent, &stat); err != nil {
			return fmt.Errorf("inspect log path parent %q: %w", parent, err)
		}
		if err := validateLogPathParent(stat); err != nil {
			return fmt.Errorf("log path parent %q: %w", parent, err)
		}
		next := filepath.Dir(parent)
		if next == parent {
			return nil
		}
		parent = next
	}
}

func validateLogPathParent(stat unix.Stat_t) error {
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("path parent is not a directory")
	}
	euid := uint32(os.Geteuid())
	if stat.Uid != 0 && stat.Uid != euid {
		return errors.New("path parent has an unrelated owner")
	}
	if stat.Mode&0o022 != 0 && stat.Mode&unix.S_ISVTX == 0 {
		return errors.New("path parent is writable by group or other users without the sticky bit")
	}
	return nil
}

func validateLogDirectory(stat unix.Stat_t) (bool, uint32, error) {
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return false, 0, errors.New("target is not a directory")
	}
	euid := uint32(os.Geteuid())
	if stat.Uid != 0 && stat.Uid != euid {
		return false, 0, errors.New("directory has an unrelated owner")
	}
	if stat.Mode&0o002 != 0 {
		return false, 0, errors.New("directory is writable by other users")
	}
	shared := stat.Mode&0o020 != 0
	if shared {
		if stat.Mode&unix.S_ISVTX != 0 {
			return false, 0, errors.New("sticky shared log directory cannot rotate entries owned by another identity")
		}
		if stat.Mode&unix.S_ISGID == 0 {
			return false, 0, fmt.Errorf("group-writable directory mode %#o must be setgid", stat.Mode)
		}
		if stat.Mode&0o050 != 0o050 {
			return false, 0, errors.New("shared directory group needs read and search permissions")
		}
		if euid != 0 {
			member, err := currentProcessInGroup(stat.Gid)
			if err != nil {
				return false, 0, err
			}
			if !member {
				return false, 0, fmt.Errorf("process is not a member of shared log group ID %d", stat.Gid)
			}
		}
		return true, sharedLogFileMode, nil
	}
	if euid != 0 && stat.Uid == euid && stat.Mode&0o300 != 0o300 {
		return false, 0, errors.New("directory owner needs write and search permissions")
	}
	if stat.Mode&0o070 != 0 {
		member, err := currentProcessInGroup(stat.Gid)
		if err != nil {
			return false, 0, err
		}
		if !member {
			return false, 0, fmt.Errorf("process is not a member of private log read group ID %d", stat.Gid)
		}
	}
	return false, privateLogFileMode, nil
}

func currentProcessInGroup(gid uint32) (bool, error) {
	groups, err := os.Getgroups()
	if err != nil {
		return false, fmt.Errorf("inspect process groups: %w", err)
	}
	return groupListContains(gid, os.Getegid(), groups), nil
}

func groupListContains(want uint32, effective int, supplementary []int) bool {
	if effective >= 0 && uint32(effective) == want {
		return true
	}
	for _, group := range supplementary {
		if group >= 0 && uint32(group) == want {
			return true
		}
	}
	return false
}

func (d *logDirectory) close() error {
	if d == nil || d.fd < 0 {
		return nil
	}
	err := unix.Close(d.fd)
	d.fd = -1
	return err
}

func (d *logDirectory) openFile(name string, flags int, create bool) (*os.File, fileSnapshot, error) {
	if err := validateLogName(name); err != nil {
		return nil, fileSnapshot{}, err
	}
	created := false
	var fd int
	var err error
	if create {
		fd, err = unix.Openat(d.fd, name, flags|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, d.fileMode)
		if err == nil {
			created = true
		} else if !errors.Is(err, unix.EEXIST) {
			return nil, fileSnapshot{}, err
		}
	}
	if !created {
		fd, err = unix.Openat(d.fd, name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
		if err != nil {
			return nil, fileSnapshot{}, err
		}
	}
	file := os.NewFile(uintptr(fd), filepath.Join(d.configured, name))
	if file == nil {
		_ = unix.Close(fd)
		return nil, fileSnapshot{}, errors.New("create log file descriptor")
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = file.Close()
		}
	}()
	if created {
		if err := unix.Fchmod(fd, d.fileMode); err != nil {
			return nil, fileSnapshot{}, fmt.Errorf("set exact log file permissions: %w", err)
		}
	}
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		return nil, fileSnapshot{}, fmt.Errorf("inspect log file descriptor: %w", err)
	}
	if err := d.validateFile(name, opened); err != nil {
		return nil, fileSnapshot{}, err
	}
	var entry unix.Stat_t
	if err := unix.Fstatat(d.fd, name, &entry, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, fileSnapshot{}, fmt.Errorf("recheck log file entry: %w", err)
	}
	if !sameStatIdentity(opened, entry) {
		return nil, fileSnapshot{}, fmt.Errorf("log file %q changed while opening", name)
	}
	closeOnError = false
	return file, snapshotFromStat(opened), nil
}

func (d *logDirectory) createFileExclusive(name string, flags int) (*os.File, fileSnapshot, error) {
	if err := validateLogName(name); err != nil {
		return nil, fileSnapshot{}, err
	}
	fd, err := unix.Openat(d.fd, name, flags|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, d.fileMode)
	if err != nil {
		return nil, fileSnapshot{}, err
	}
	file := os.NewFile(uintptr(fd), filepath.Join(d.configured, name))
	if file == nil {
		_ = unix.Close(fd)
		return nil, fileSnapshot{}, errors.New("create exclusive log file descriptor")
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = file.Close()
		}
	}()
	if err := unix.Fchmod(fd, d.fileMode); err != nil {
		return nil, fileSnapshot{}, fmt.Errorf("set exact exclusive log file permissions: %w", err)
	}
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		return nil, fileSnapshot{}, fmt.Errorf("inspect exclusive log file descriptor: %w", err)
	}
	if err := d.validateFile(name, opened); err != nil {
		return nil, fileSnapshot{}, err
	}
	var entry unix.Stat_t
	if err := unix.Fstatat(d.fd, name, &entry, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, fileSnapshot{}, fmt.Errorf("recheck exclusive log entry: %w", err)
	}
	if !sameStatIdentity(opened, entry) {
		return nil, fileSnapshot{}, fmt.Errorf("exclusive log object %q changed while creating", name)
	}
	closeOnError = false
	return file, snapshotFromStat(opened), nil
}

func (d *logDirectory) validateFile(name string, stat unix.Stat_t) error {
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("log object %q must be a regular file", name)
	}
	if stat.Nlink != 1 {
		return fmt.Errorf("log object %q must have exactly one hard link", name)
	}
	if stat.Mode&0o7777 != d.fileMode {
		return fmt.Errorf("log object %q has unsafe mode %#o; want %#o", name, stat.Mode&0o7777, d.fileMode)
	}
	if d.shared {
		if stat.Gid != d.gid {
			return fmt.Errorf("shared log object %q has the wrong group", name)
		}
		allowed, err := sharedLogOwnerAllowed(stat.Uid, d.gid)
		if err != nil {
			return fmt.Errorf("validate shared log object %q owner: %w", name, err)
		}
		if !allowed {
			return fmt.Errorf("shared log object %q has an unrelated owner", name)
		}
	} else {
		if stat.Uid != uint32(os.Geteuid()) {
			return fmt.Errorf("private log object %q has an unrelated owner", name)
		}
		member, err := currentProcessInGroup(stat.Gid)
		if err != nil {
			return fmt.Errorf("validate private log object %q group: %w", name, err)
		}
		if !member {
			return fmt.Errorf("private log object %q has an unrelated group", name)
		}
	}
	return nil
}

func sharedLogOwnerAllowed(uid, gid uint32) (bool, error) {
	if uid == 0 || uid == uint32(os.Geteuid()) {
		return true, nil
	}
	account, err := user.LookupId(strconv.FormatUint(uint64(uid), 10))
	if err != nil {
		return false, fmt.Errorf("lookup user ID %d: %w", uid, err)
	}
	if account.Gid == strconv.FormatUint(uint64(gid), 10) {
		return true, nil
	}
	groupIDs, err := account.GroupIds()
	if err != nil {
		return false, fmt.Errorf("lookup groups for user ID %d: %w", uid, err)
	}
	for _, groupID := range groupIDs {
		parsed, parseErr := strconv.ParseUint(groupID, 10, 32)
		if parseErr != nil {
			return false, fmt.Errorf("parse group ID %q for user ID %d: %w", groupID, uid, parseErr)
		}
		if uint32(parsed) == gid {
			return true, nil
		}
	}
	return false, nil
}

func (d *logDirectory) statFile(name string) (fileSnapshot, error) {
	if err := validateLogName(name); err != nil {
		return fileSnapshot{}, err
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(d.fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fileSnapshot{}, err
	}
	if err := d.validateFile(name, stat); err != nil {
		return fileSnapshot{}, err
	}
	return snapshotFromStat(stat), nil
}

func (d *logDirectory) verifyFile(name string, expected fileSnapshot) error {
	current, err := d.statFile(name)
	if err != nil {
		return err
	}
	if current.identity != expected.identity || current.links != expected.links || current.mode != expected.mode || current.uid != expected.uid || current.gid != expected.gid {
		return fmt.Errorf("log object %q changed after inspection", name)
	}
	return nil
}

func (d *logDirectory) entryExists(name string) (bool, error) {
	var stat unix.Stat_t
	err := unix.Fstatat(d.fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func validateLogName(name string) error {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name || strings.ContainsRune(name, filepath.Separator) {
		return fmt.Errorf("invalid log object name %q", name)
	}
	return nil
}

func snapshotFromStat(stat unix.Stat_t) fileSnapshot {
	return fileSnapshot{
		identity: identityFromStat(stat),
		mode:     stat.Mode,
		uid:      stat.Uid,
		gid:      stat.Gid,
		links:    uint64(stat.Nlink),
		size:     stat.Size,
		modTime:  stat.Mtim.Sec,
	}
}

func identityFromStat(stat unix.Stat_t) logIdentity {
	return logIdentity{device: uint64(stat.Dev), inode: stat.Ino}
}

func sameStatIdentity(left, right unix.Stat_t) bool {
	return left.Dev == right.Dev && left.Ino == right.Ino
}
