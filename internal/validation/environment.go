package validation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Environment is the local operating-system boundary used by Validator.
type Environment interface {
	CheckUser(name string) error
	CheckExecutable(ctx context.Context, path string) error
	CheckWritableTarget(path, runAs string) error
	CheckSecretFile(path, runAs string) error
	CheckCronPath(path string) error
}

type runAsExecutableEnvironment interface {
	CheckExecutableAs(ctx context.Context, path, runAs string) error
}

// OSEnvironment checks the host filesystem and user database. Permission
// decisions are based on the intended user's IDs and file metadata, rather
// than the privileges of the validating process. These are point-in-time
// validation checks; later execution is not atomic with validation.
type OSEnvironment struct{}

func (OSEnvironment) CheckUser(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("user name is required")
	}
	if _, err := user.Lookup(name); err != nil {
		return fmt.Errorf("lookup user %q: %w", name, err)
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
func (OSEnvironment) CheckExecutableAs(ctx context.Context, path, runAs string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("executable path %q must be absolute", path)
	}
	if err := rejectSymlinkComponents(path); err != nil {
		return fmt.Errorf("executable path %q is unsafe: %w", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat executable %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("executable %q must be a regular file", path)
	}
	identity, err := lookupIdentity(runAs)
	if err != nil {
		return err
	}
	if !identity.allows(info, permissionExecute) {
		return fmt.Errorf("executable %q is not executable by intended user %q", path, runAs)
	}
	if err := identity.checkParentTraversal(path); err != nil {
		return fmt.Errorf("intended user %q cannot traverse executable path %q: %w", runAs, path, err)
	}
	if err := exec.CommandContext(ctx, path, "--version").Run(); err != nil {
		return fmt.Errorf("execute %q --version: %w", path, err)
	}
	return nil
}

func (OSEnvironment) CheckWritableTarget(path, runAs string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("target path %q must be absolute", path)
	}
	if err := rejectSymlinkComponents(path); err != nil {
		return fmt.Errorf("writable target path %q is unsafe: %w", path, err)
	}
	identity, err := lookupIdentity(runAs)
	if err != nil {
		return err
	}

	candidate := filepath.Clean(path)
	missing := false
	for {
		info, statErr := os.Stat(candidate)
		if statErr == nil {
			if !info.IsDir() {
				return fmt.Errorf("writable target %q must be a directory", candidate)
			}
			if !identity.allows(info, permissionWrite) || !identity.allows(info, permissionExecute) {
				if missing {
					return fmt.Errorf("nearest existing parent %q is not writable by intended user %q", candidate, runAs)
				}
				return fmt.Errorf("directory %q is not writable by intended user %q", candidate, runAs)
			}
			if err := identity.checkParentTraversal(candidate); err != nil {
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

func (OSEnvironment) CheckSecretFile(path, runAs string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("secret file path %q must be absolute", path)
	}
	if err := rejectSymlinkComponents(path); err != nil {
		return fmt.Errorf("secret file path %q is unsafe: %w", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat secret file %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("secret file %q must be a regular file", path)
	}
	if info.Mode().Perm()&0o007 != 0 {
		return fmt.Errorf("secret file %q has other-user permission bits", path)
	}
	identity, err := lookupIdentity(runAs)
	if err != nil {
		return err
	}
	if !identity.allows(info, permissionRead) {
		return fmt.Errorf("secret file %q is not readable by intended user %q", path, runAs)
	}
	if err := identity.checkParentTraversal(path); err != nil {
		return fmt.Errorf("intended user %q cannot traverse secret file path %q: %w", runAs, path, err)
	}
	return nil
}

func (OSEnvironment) CheckCronPath(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("cron path %q must be absolute", path)
	}
	if strings.ContainsAny(path, "\n\x00") {
		return fmt.Errorf("cron path %q must not contain a newline or NUL", path)
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
	identity := userIdentity{uid: uid, groups: make(map[uint32]struct{})}
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
		next := filepath.Dir(parent)
		if next == parent {
			return nil
		}
		parent = next
	}
}
