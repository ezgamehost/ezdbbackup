package validation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Environment is the local operating-system boundary used by Validator.
type Environment interface {
	CheckUser(name string) error
	CheckRunIdentity(name string) error
	CheckExecutable(ctx context.Context, path string) error
	CheckRuntimeExecutable(ctx context.Context, path, runAs string) error
	CheckConfigFile(path, runAs string) error
	CheckWritableTarget(path, runAs string) error
	CheckLoggingTarget(path string, runAs []string) error
	CheckSecretFile(path, runAs string) error
	CheckCronPath(path string) error
}

type runAsExecutableEnvironment interface {
	CheckExecutableAs(ctx context.Context, path, runAs string) error
}

type stagingTargetEnvironment interface {
	CheckStagingTarget(path, runAs string) error
}

// OSEnvironment checks the host filesystem and user database. Permission
// decisions are based on the intended user's IDs and file metadata, rather
// than the privileges of the validating process. These are point-in-time
// validation checks; later execution is not atomic with validation.
type OSEnvironment struct {
	beforePathRecheck func(resolved string)
}

func (OSEnvironment) CheckUser(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("user name is required")
	}
	if _, err := user.Lookup(name); err != nil {
		return fmt.Errorf("lookup user %q: %w", name, err)
	}
	return nil
}

// CheckRunIdentity prevents a backup from executing configured programs or
// reading credentials with privileges different from its scheduled identity.
func (OSEnvironment) CheckRunIdentity(name string) error {
	identity, err := lookupIdentity(name)
	if err != nil {
		return err
	}
	if got := uint32(os.Geteuid()); got != identity.uid {
		return fmt.Errorf("effective user ID %d does not match configured run_as user ID %d", got, identity.uid)
	}
	return nil
}

func (e OSEnvironment) CheckExecutable(ctx context.Context, path string) error {
	current, err := user.Current()
	if err != nil {
		return fmt.Errorf("lookup current user: %w", err)
	}
	return e.CheckExecutableAs(ctx, path, current.Username)
}

// CheckExecutableAs validates execution permission for the intended cron user.
func (e OSEnvironment) CheckExecutableAs(ctx context.Context, path, runAs string) error {
	return e.checkExecutableAs(ctx, path, runAs, "--version")
}

func (e OSEnvironment) CheckRuntimeExecutable(ctx context.Context, path, runAs string) error {
	return e.checkExecutableAs(ctx, path, runAs, "version")
}

func (e OSEnvironment) checkExecutableAs(ctx context.Context, path, runAs, versionArgument string) error {
	resolved, stat, closeTarget, err := e.inspectResolvedTarget(path)
	if err != nil {
		return fmt.Errorf("inspect executable %q: %w", path, err)
	}
	defer closeTarget()
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("executable %q must be a regular file", path)
	}
	if stat.Mode&0o022 != 0 {
		return fmt.Errorf("executable %q is writable by group or other users", path)
	}
	identity, err := lookupIdentity(runAs)
	if err != nil {
		return err
	}
	if !identity.allowsStat(stat, false, permissionExecute) {
		return fmt.Errorf("executable %q is not executable by intended user %q", path, runAs)
	}
	if err := identity.checkLexicalAndResolvedTraversal(path, resolved); err != nil {
		return fmt.Errorf("intended user %q cannot traverse executable path %q: %w", runAs, path, err)
	}
	if stat.Uid != 0 && stat.Uid != identity.uid {
		return fmt.Errorf("executable %q has an unrelated owner", path)
	}
	credential, err := versionProbeCredential(identity, uint32(os.Geteuid()))
	if err != nil {
		return fmt.Errorf("prepare executable %q version probe: %w", path, err)
	}
	command := exec.CommandContext(ctx, resolved, versionArgument)
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	if credential != nil {
		command.SysProcAttr = &syscall.SysProcAttr{Credential: credential}
	}
	if err := command.Run(); err != nil {
		return fmt.Errorf("execute %q %s: %w", path, versionArgument, err)
	}
	return nil
}

func (e OSEnvironment) CheckWritableTarget(path, runAs string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("target path %q must be a clean absolute path", path)
	}
	identity, err := lookupIdentity(runAs)
	if err != nil {
		return err
	}

	candidate := path
	missing := false
	for {
		resolved, stat, closeTarget, statErr := e.inspectResolvedTarget(candidate)
		if statErr == nil {
			defer closeTarget()
			if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
				return fmt.Errorf("writable target %q must be a directory", candidate)
			}
			if !identity.allowsStat(stat, true, permissionWrite) || !identity.allowsStat(stat, true, permissionExecute) {
				if missing {
					return fmt.Errorf("nearest existing parent %q is not writable by intended user %q", candidate, runAs)
				}
				return fmt.Errorf("directory %q is not writable by intended user %q", candidate, runAs)
			}
			if err := identity.checkLexicalAndResolvedTraversal(candidate, resolved); err != nil {
				return fmt.Errorf("intended user %q cannot traverse writable target %q: %w", runAs, candidate, err)
			}
			return nil
		}
		if !os.IsNotExist(statErr) {
			return fmt.Errorf("stat writable target %q: %w", candidate, statErr)
		}
		missing = true
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return fmt.Errorf("find nearest existing parent for %q", path)
		}
		candidate = parent
	}
}

// CheckStagingTarget applies the writable-target checks plus replacement
// protections needed for sensitive staged artifacts. Private user/root-owned
// directories are accepted. Shared writable ancestors must be sticky, and
// every existing target ancestor must be owned by root or run_as so the
// configured directory cannot be renamed out from under runtime identity
// checks.
func (e OSEnvironment) CheckStagingTarget(path, runAs string) error {
	if err := rejectSymlinkComponents(path); err != nil {
		return fmt.Errorf("staging target path %q is unsafe: %w", path, err)
	}
	if err := e.CheckWritableTarget(path, runAs); err != nil {
		return err
	}
	identity, err := lookupIdentity(runAs)
	if err != nil {
		return err
	}
	candidate := filepath.Clean(path)
	for {
		_, statErr := os.Stat(candidate)
		if statErr == nil {
			break
		}
		if !os.IsNotExist(statErr) {
			return fmt.Errorf("inspect staging target: %w", statErr)
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return errors.New("find nearest existing staging parent")
		}
		candidate = parent
	}
	for {
		info, err := os.Stat(candidate)
		if err != nil {
			return fmt.Errorf("inspect staging ancestor: %w", err)
		}
		if err := validateStagingDirectory(candidate, info, identity); err != nil {
			return err
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return nil
		}
		candidate = parent
	}
}

func validateStagingDirectory(path string, info os.FileInfo, identity userIdentity) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("inspect staging directory ownership")
	}
	if stat.Uid != 0 && stat.Uid != identity.uid {
		return fmt.Errorf("staging directory %q has an unrelated owner", path)
	}
	if info.Mode().Perm()&0o022 != 0 && info.Mode()&os.ModeSticky == 0 {
		return fmt.Errorf("shared writable staging directory %q must have the sticky bit", path)
	}
	return nil
}

func (e OSEnvironment) CheckSecretFile(path, runAs string) error {
	return e.checkSensitiveFile(path, runAs, "secret file")
}

func (e OSEnvironment) CheckConfigFile(path, runAs string) error {
	return e.checkSensitiveFile(path, runAs, "configuration file")
}

func (e OSEnvironment) checkSensitiveFile(path, runAs, label string) error {
	resolved, stat, closeTarget, err := e.inspectResolvedTarget(path)
	if err != nil {
		return fmt.Errorf("inspect %s %q: %w", label, path, err)
	}
	defer closeTarget()
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("%s %q must be a regular file", label, path)
	}
	if stat.Nlink != 1 {
		return fmt.Errorf("%s %q must have exactly one hard link", label, path)
	}
	if stat.Mode&0o007 != 0 {
		return fmt.Errorf("%s %q has other-user permission bits", label, path)
	}
	if stat.Mode&0o022 != 0 {
		return fmt.Errorf("%s %q is writable by group or other users", label, path)
	}
	identity, err := lookupIdentity(runAs)
	if err != nil {
		return err
	}
	if !identity.allowsStat(stat, false, permissionRead) {
		return fmt.Errorf("%s %q is not readable by intended user %q", label, path, runAs)
	}
	if err := identity.checkLexicalAndResolvedTraversal(path, resolved); err != nil {
		return fmt.Errorf("intended user %q cannot traverse %s path %q: %w", runAs, label, path, err)
	}
	if stat.Uid != 0 && stat.Uid != identity.uid {
		return fmt.Errorf("%s %q has an unrelated owner", label, path)
	}
	return nil
}

func (e OSEnvironment) CheckLoggingTarget(path string, runAs []string) error {
	identities := make([]userIdentity, 0, len(runAs))
	for _, name := range runAs {
		identity, err := lookupIdentity(name)
		if err != nil {
			return err
		}
		// Preserve every account's group set. Distinct account names may map to
		// the same UID while retaining different supplementary memberships.
		identities = append(identities, identity)
	}
	if len(identities) == 0 {
		return nil
	}
	resolved, stat, closeTarget, err := e.inspectResolvedTarget(path)
	if errors.Is(err, os.ErrNotExist) {
		if distinctUIDCount(identities) > 1 {
			return errors.New("multiple run_as identities require an existing setgid shared log directory")
		}
		if err := e.CheckWritableTarget(path, runAs[0]); err != nil {
			return err
		}
		return e.checkMissingLogParent(path, identities[0])
	}
	if err != nil {
		return fmt.Errorf("inspect log directory: %w", err)
	}
	defer closeTarget()
	if err := validateLogTargetTraversal(path, resolved, identities); err != nil {
		return err
	}
	return validateLogTargetDirectory(stat, identities)
}

func (e OSEnvironment) checkMissingLogParent(path string, identity userIdentity) error {
	candidate := filepath.Dir(path)
	for {
		_, stat, closeTarget, err := e.inspectResolvedTarget(candidate)
		if err == nil {
			defer closeTarget()
			if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
				return fmt.Errorf("nearest existing log parent %q is not a directory", candidate)
			}
			if stat.Uid != 0 && stat.Uid != identity.uid {
				return fmt.Errorf("nearest existing log parent %q has an unrelated owner", candidate)
			}
			if stat.Mode&0o022 != 0 && stat.Mode&unix.S_ISVTX == 0 {
				return fmt.Errorf("nearest existing log parent %q is writable by group or other users without the sticky bit", candidate)
			}
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect nearest existing log parent %q: %w", candidate, err)
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return fmt.Errorf("find nearest existing log parent for %q", path)
		}
		candidate = parent
	}
}

func validateLogTargetTraversal(lexical, resolved string, identities []userIdentity) error {
	for _, identity := range identities {
		if err := identity.checkLexicalAndResolvedTraversal(lexical, resolved); err != nil {
			return fmt.Errorf("run_as user ID %d cannot traverse log directory path: %w", identity.uid, err)
		}
	}
	return nil
}

func validateLogTargetDirectory(stat syscall.Stat_t, identities []userIdentity) error {
	if stat.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		return errors.New("log target must be a directory")
	}
	if stat.Mode&0o002 != 0 {
		return errors.New("log directory must not be writable by other users")
	}
	multipleUIDs := distinctUIDCount(identities) > 1
	if multipleUIDs && stat.Uid != 0 {
		return errors.New("multiple run_as identities require a root-owned shared log directory")
	}
	ownerAllowed := stat.Uid == 0
	for _, identity := range identities {
		if identity.uid == stat.Uid {
			ownerAllowed = true
		}
	}
	if !ownerAllowed {
		return errors.New("log directory has an unrelated owner")
	}
	if multipleUIDs {
		if stat.Mode&syscall.S_ISGID == 0 || stat.Mode&0o070 != 0o070 {
			return errors.New("multiple run_as identities require a setgid log directory with group read, write, and search permissions")
		}
		for _, identity := range identities {
			if identity.uid == 0 {
				continue
			}
			if _, member := identity.groups[stat.Gid]; !member {
				return fmt.Errorf("run_as user ID %d is not a member of shared log group ID %d", identity.uid, stat.Gid)
			}
		}
		return nil
	}
	for _, identity := range identities {
		if !identity.allowsStat(stat, true, permissionRead) || !identity.allowsStat(stat, true, permissionWrite) || !identity.allowsStat(stat, true, permissionExecute) {
			return fmt.Errorf("log directory is not readable, writable, and searchable by run_as user ID %d", identity.uid)
		}
	}
	if stat.Mode&0o020 != 0 && (stat.Mode&syscall.S_ISGID == 0 || stat.Mode&0o070 != 0o070) {
		return errors.New("group-writable log directory must be setgid with group read, write, and search permissions")
	}
	if stat.Mode&0o070 != 0 {
		for _, identity := range identities {
			if identity.uid == 0 {
				continue
			}
			if _, member := identity.groups[stat.Gid]; !member {
				return fmt.Errorf("run_as user ID %d is not a member of log group ID %d", identity.uid, stat.Gid)
			}
		}
	}
	return nil
}

func distinctUIDCount(identities []userIdentity) int {
	seen := make(map[uint32]struct{}, len(identities))
	for _, identity := range identities {
		seen[identity.uid] = struct{}{}
	}
	return len(seen)
}

func (e OSEnvironment) inspectResolvedTarget(path string) (string, syscall.Stat_t, func() error, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", syscall.Stat_t{}, func() error { return nil }, fmt.Errorf("path %q must be a clean absolute path", path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", syscall.Stat_t{}, func() error { return nil }, err
	}
	fd, err := unix.Open(resolved, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", syscall.Stat_t{}, func() error { return nil }, err
	}
	closeTarget := func() error { return unix.Close(fd) }
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		_ = closeTarget()
		return "", syscall.Stat_t{}, func() error { return nil }, err
	}
	if e.beforePathRecheck != nil {
		e.beforePathRecheck(resolved)
	}
	currentResolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		_ = closeTarget()
		return "", syscall.Stat_t{}, func() error { return nil }, fmt.Errorf("re-resolve inspected path: %w", err)
	}
	if currentResolved != resolved {
		_ = closeTarget()
		return "", syscall.Stat_t{}, func() error { return nil }, errors.New("resolved target changed while it was inspected")
	}
	var current unix.Stat_t
	if err := unix.Lstat(resolved, &current); err != nil {
		_ = closeTarget()
		return "", syscall.Stat_t{}, func() error { return nil }, err
	}
	if opened.Dev != current.Dev || opened.Ino != current.Ino || opened.Mode&unix.S_IFMT == unix.S_IFLNK {
		_ = closeTarget()
		return "", syscall.Stat_t{}, func() error { return nil }, errors.New("resolved target changed while it was inspected")
	}
	return resolved, syscall.Stat_t{
		Dev: opened.Dev, Ino: opened.Ino, Nlink: opened.Nlink, Mode: opened.Mode,
		Uid: opened.Uid, Gid: opened.Gid, Rdev: opened.Rdev, Size: opened.Size,
	}, closeTarget, nil
}

func (OSEnvironment) CheckCronPath(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("cron path %q must be absolute", path)
	}
	if strings.ContainsAny(path, "\r\n\x00%") {
		return fmt.Errorf("cron path %q must not contain a newline, NUL, or cron-special %%", path)
	}
	return nil
}

func rejectSymlinkComponents(path string) error {
	clean := filepath.Clean(path)
	if path != clean {
		return fmt.Errorf("path %q must be a clean absolute path", path)
	}
	current := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(clean, current), current) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect path component %q: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path component %q is a symbolic link", current)
		}
	}
	return nil
}

type permission uint8

const (
	permissionExecute permission = 1
	permissionWrite   permission = 2
	permissionRead    permission = 4
)

type userIdentity struct {
	uid    uint32
	gid    uint32
	groups map[uint32]struct{}
}

func lookupIdentity(name string) (userIdentity, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return userIdentity{}, fmt.Errorf("lookup user %q: %w", name, err)
	}
	uid, err := parseID("user", u.Uid)
	if err != nil {
		return userIdentity{}, err
	}
	gid, err := parseID("group", u.Gid)
	if err != nil {
		return userIdentity{}, err
	}
	identity := userIdentity{uid: uid, gid: gid, groups: make(map[uint32]struct{})}
	groupIDs, err := u.GroupIds()
	if err != nil {
		return userIdentity{}, fmt.Errorf("lookup groups for user %q: %w", name, err)
	}
	for _, value := range groupIDs {
		gid, parseErr := parseID("group", value)
		if parseErr != nil {
			return userIdentity{}, parseErr
		}
		identity.groups[gid] = struct{}{}
	}
	return identity, nil
}

func versionProbeCredential(identity userIdentity, invokingUID uint32) (*syscall.Credential, error) {
	if invokingUID == identity.uid {
		return nil, nil
	}
	if invokingUID != 0 {
		return nil, fmt.Errorf("cannot safely execute as user ID %d from invoking user ID %d", identity.uid, invokingUID)
	}
	groups := make([]uint32, 0, len(identity.groups))
	for group := range identity.groups {
		groups = append(groups, group)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i] < groups[j] })
	return &syscall.Credential{Uid: identity.uid, Gid: identity.gid, Groups: groups}, nil
}

func parseID(kind, value string) (uint32, error) {
	id, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("parse %s ID %q: %w", kind, value, err)
	}
	return uint32(id), nil
}

func (u userIdentity) allows(info os.FileInfo, requested permission) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	mode := info.Mode().Perm()
	if u.uid == 0 {
		if requested != permissionExecute || info.IsDir() {
			return true
		}
		return mode&0o111 != 0
	}
	var bits os.FileMode
	switch {
	case u.uid == stat.Uid:
		bits = (mode >> 6) & 0o7
	default:
		if _, member := u.groups[stat.Gid]; member {
			bits = (mode >> 3) & 0o7
		} else {
			bits = mode & 0o7
		}
	}
	return bits&os.FileMode(requested) != 0
}

func (u userIdentity) allowsStat(stat syscall.Stat_t, directory bool, requested permission) bool {
	mode := os.FileMode(stat.Mode & 0o777)
	if u.uid == 0 {
		if requested != permissionExecute || directory {
			return true
		}
		return mode&0o111 != 0
	}
	var bits os.FileMode
	switch {
	case u.uid == stat.Uid:
		bits = (mode >> 6) & 0o7
	default:
		if _, member := u.groups[stat.Gid]; member {
			bits = (mode >> 3) & 0o7
		} else {
			bits = mode & 0o7
		}
	}
	return bits&os.FileMode(requested) != 0
}

func (u userIdentity) checkLexicalAndResolvedTraversal(lexical, resolved string) error {
	if err := u.checkParentTraversal(lexical); err != nil {
		return fmt.Errorf("lexical path traversal: %w", err)
	}
	if resolved != lexical {
		if err := u.checkParentTraversal(resolved); err != nil {
			return fmt.Errorf("resolved path traversal: %w", err)
		}
	}
	return nil
}

func (u userIdentity) checkParentTraversal(path string) error {
	parent := filepath.Dir(filepath.Clean(path))
	for {
		info, err := os.Stat(parent)
		if err != nil {
			return fmt.Errorf("stat parent directory %q: %w", parent, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("parent path %q is not a directory", parent)
		}
		if !u.allows(info, permissionExecute) {
			return fmt.Errorf("parent directory %q is not searchable", parent)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("inspect parent directory %q ownership", parent)
		}
		if stat.Uid != 0 && stat.Uid != u.uid {
			return fmt.Errorf("parent directory %q has an unrelated owner", parent)
		}
		if info.Mode().Perm()&0o022 != 0 && info.Mode()&os.ModeSticky == 0 {
			return fmt.Errorf("parent directory %q is writable by group or other users without the sticky bit", parent)
		}
		next := filepath.Dir(parent)
		if next == parent {
			return nil
		}
		parent = next
	}
}
