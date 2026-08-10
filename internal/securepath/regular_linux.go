// Package securepath pins trusted Linux filesystem objects to descriptors and
// records the metadata needed to reject later pathname substitution.
package securepath

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Access is the permission an intended runtime identity needs on a file.
type Access uint8

const (
	AccessExecute Access = 1
	AccessRead    Access = 4
)

// Identity is the ordinary account identity used to validate ownership,
// access, and canonical path traversal.
type Identity struct {
	UID    uint32
	GID    uint32
	Groups map[uint32]struct{}
}

// Metadata is an immutable snapshot of a pinned regular file.
type Metadata struct {
	Device uint64
	Inode  uint64
	Links  uint64
	Mode   uint32
	UID    uint32
	GID    uint32
	Size   int64
}

// Source associates a requested pathname and its trusted canonical pathname
// with the exact regular file descriptor metadata used by a caller.
type Source struct {
	RequestedPath string
	CanonicalPath string
	Metadata      Metadata
	policy        Policy
}

// Policy describes a sensitive regular file opened through a trusted
// canonical path.
type Policy struct {
	Label                  string
	Identity               Identity
	Access                 Access
	MaxBytes               int64
	RequireSingleLink      bool
	RejectOtherPermissions bool
	PathOnly               bool
}

// LookupIdentity resolves a local account and its complete login group set.
func LookupIdentity(name string) (Identity, error) {
	account, err := user.Lookup(name)
	if err != nil {
		return Identity{}, fmt.Errorf("lookup user %q: %w", name, err)
	}
	uid, err := parseID("user", account.Uid)
	if err != nil {
		return Identity{}, err
	}
	gid, err := parseID("group", account.Gid)
	if err != nil {
		return Identity{}, err
	}
	identity := Identity{UID: uid, GID: gid, Groups: make(map[uint32]struct{})}
	groupIDs, err := account.GroupIds()
	if err != nil {
		return Identity{}, fmt.Errorf("lookup groups for user %q: %w", name, err)
	}
	for _, value := range groupIDs {
		groupID, parseErr := parseID("group", value)
		if parseErr != nil {
			return Identity{}, parseErr
		}
		identity.Groups[groupID] = struct{}{}
	}
	identity.Groups[gid] = struct{}{}
	return identity, nil
}

// CurrentIdentity returns the process filesystem identity and group set.
func CurrentIdentity() (Identity, error) {
	groups, err := os.Getgroups()
	if err != nil {
		return Identity{}, fmt.Errorf("inspect process groups: %w", err)
	}
	identity := Identity{
		UID:    uint32(os.Geteuid()),
		GID:    uint32(os.Getegid()),
		Groups: make(map[uint32]struct{}, len(groups)+1),
	}
	identity.Groups[identity.GID] = struct{}{}
	for _, group := range groups {
		if group < 0 {
			return Identity{}, errors.New("inspect process groups: negative group ID")
		}
		identity.Groups[uint32(group)] = struct{}{}
	}
	return identity, nil
}

// OpenRegular resolves path once, walks the canonical path without following
// another link, and returns the exact validated descriptor.
func OpenRegular(path string, policy Policy) (*os.File, Source, error) {
	label := policyLabel(policy)
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, Source{}, fmt.Errorf("%s path %q must be a clean absolute path", label, path)
	}
	if err := validateLexicalPath(path, policy.Identity); err != nil {
		return nil, Source{}, fmt.Errorf("inspect lexical %s path %q: %w", label, path, err)
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, Source{}, fmt.Errorf("resolve %s %q: %w", label, path, err)
	}
	canonical = filepath.Clean(canonical)
	file, metadata, err := openCanonical(canonical, policy)
	if err != nil {
		return nil, Source{}, fmt.Errorf("open %s %q: %w", label, path, err)
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = file.Close()
		}
	}()

	currentCanonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, Source{}, fmt.Errorf("re-resolve %s %q: %w", label, path, err)
	}
	if filepath.Clean(currentCanonical) != canonical {
		return nil, Source{}, fmt.Errorf("%s path changed while opening", label)
	}
	if err := validateLexicalPath(path, policy.Identity); err != nil {
		return nil, Source{}, fmt.Errorf("recheck lexical %s path %q: %w", label, path, err)
	}
	var entry unix.Stat_t
	if err := unix.Lstat(canonical, &entry); err != nil {
		return nil, Source{}, fmt.Errorf("recheck canonical %s entry: %w", label, err)
	}
	if !metadata.matches(entry) {
		return nil, Source{}, fmt.Errorf("canonical %s entry changed while opening", label)
	}

	source := Source{
		RequestedPath: path,
		CanonicalPath: canonical,
		Metadata:      metadata,
		policy:        clonePolicy(policy),
	}
	closeOnError = false
	return file, source, nil
}

// ReadAll reads through the pinned descriptor with a strict byte limit and
// rechecks immutable metadata before returning the bytes.
func ReadAll(file *os.File, source Source, maxBytes int64) ([]byte, error) {
	if file == nil {
		return nil, errors.New("pinned file descriptor is required")
	}
	if maxBytes <= 0 {
		return nil, errors.New("positive file size limit is required")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		clear(data)
		return nil, fmt.Errorf("%s is too large; maximum is %d bytes", policyLabel(source.policy), maxBytes)
	}
	var current unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &current); err != nil {
		clear(data)
		return nil, fmt.Errorf("recheck pinned %s descriptor: %w", policyLabel(source.policy), err)
	}
	if !source.Metadata.matches(current) || current.Size != int64(len(data)) {
		clear(data)
		return nil, fmt.Errorf("pinned %s changed while reading", policyLabel(source.policy))
	}
	return data, nil
}

// Recheck verifies that the canonical trusted path still names the exact
// metadata captured by OpenRegular.
func Recheck(source Source) error {
	return ValidateFor(source, source.policy.Identity, source.policy.Access)
}

// RecheckPath proves both that the trusted canonical target retains its exact
// metadata and that the originally requested lexical path still resolves to
// it through trusted, non-replaceable symlink entries. It is used immediately
// before pathname execution so shebang scripts retain their normal argv[0].
func RecheckPath(source Source) error {
	if err := validateLexicalPath(source.RequestedPath, source.policy.Identity); err != nil {
		return fmt.Errorf("recheck lexical %s path: %w", policyLabel(source.policy), err)
	}
	resolved, err := filepath.EvalSymlinks(source.RequestedPath)
	if err != nil {
		return fmt.Errorf("re-resolve %s path: %w", policyLabel(source.policy), err)
	}
	if filepath.Clean(resolved) != source.CanonicalPath {
		return fmt.Errorf("%s path association changed", policyLabel(source.policy))
	}
	if err := Recheck(source); err != nil {
		return err
	}
	if err := validateLexicalPath(source.RequestedPath, source.policy.Identity); err != nil {
		return fmt.Errorf("recheck lexical %s path after target inspection: %w", policyLabel(source.policy), err)
	}
	resolved, err = filepath.EvalSymlinks(source.RequestedPath)
	if err != nil {
		return fmt.Errorf("final re-resolve %s path: %w", policyLabel(source.policy), err)
	}
	if filepath.Clean(resolved) != source.CanonicalPath {
		return fmt.Errorf("%s path association changed", policyLabel(source.policy))
	}
	return nil
}

// ValidateFor inspects the recorded descriptor metadata and canonical path as
// an intended runtime identity without reopening the requested pathname.
func ValidateFor(source Source, identity Identity, access Access) error {
	policy := clonePolicy(source.policy)
	policy.Identity = cloneIdentity(identity)
	policy.Access = access
	policy.PathOnly = true
	file, metadata, err := openCanonical(source.CanonicalPath, policy)
	if err != nil {
		return err
	}
	closeErr := file.Close()
	if metadata != source.Metadata {
		return errors.Join(fmt.Errorf("%s identity or metadata changed", policyLabel(policy)), closeErr)
	}
	return closeErr
}

func openCanonical(path string, policy Policy) (*os.File, Metadata, error) {
	rootFD, err := unix.Open(string(filepath.Separator), unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, Metadata{}, fmt.Errorf("open canonical path root: %w", err)
	}
	fd := rootFD
	closeFD := true
	defer func() {
		if closeFD {
			_ = unix.Close(fd)
		}
	}()

	var current unix.Stat_t
	if err := unix.Fstat(fd, &current); err != nil {
		return nil, Metadata{}, fmt.Errorf("inspect canonical path root: %w", err)
	}
	components := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	remaining := 0
	for _, component := range components {
		if component != "" {
			remaining++
		}
	}
	if remaining == 0 {
		return nil, Metadata{}, errors.New("canonical regular file path cannot be the filesystem root")
	}
	if err := validateAncestor(current, policy.Identity, string(filepath.Separator)); err != nil {
		return nil, Metadata{}, err
	}

	currentPath := string(filepath.Separator)
	for _, component := range components {
		if component == "" {
			continue
		}
		remaining--
		flags := unix.O_PATH | unix.O_NOFOLLOW | unix.O_CLOEXEC
		if remaining > 0 {
			flags |= unix.O_DIRECTORY
		} else if !policy.PathOnly {
			flags = unix.O_RDONLY | unix.O_NONBLOCK | unix.O_NOFOLLOW | unix.O_CLOEXEC
		}
		nextFD, openErr := unix.Openat(fd, component, flags, 0)
		if openErr != nil {
			return nil, Metadata{}, fmt.Errorf("open canonical path component %q: %w", component, openErr)
		}
		var next unix.Stat_t
		if err := unix.Fstat(nextFD, &next); err != nil {
			_ = unix.Close(nextFD)
			return nil, Metadata{}, fmt.Errorf("inspect canonical path component %q: %w", component, err)
		}
		var entry unix.Stat_t
		if err := unix.Fstatat(fd, component, &entry, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			_ = unix.Close(nextFD)
			return nil, Metadata{}, fmt.Errorf("recheck canonical path component %q: %w", component, err)
		}
		if next.Dev != entry.Dev || next.Ino != entry.Ino || entry.Mode&unix.S_IFMT == unix.S_IFLNK {
			_ = unix.Close(nextFD)
			return nil, Metadata{}, fmt.Errorf("canonical path component %q changed while opening", component)
		}
		if err := unix.Close(fd); err != nil {
			_ = unix.Close(nextFD)
			return nil, Metadata{}, fmt.Errorf("close canonical path component: %w", err)
		}
		fd = nextFD
		current = next
		currentPath = filepath.Join(currentPath, component)
		if remaining > 0 {
			if err := validateAncestor(current, policy.Identity, currentPath); err != nil {
				return nil, Metadata{}, err
			}
		}
	}

	metadata := metadataFromStat(current)
	if err := validateTarget(metadata, policy); err != nil {
		return nil, Metadata{}, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		return nil, Metadata{}, errors.New("create pinned file descriptor")
	}
	closeFD = false
	return file, metadata, nil
}

func validateLexicalPath(path string, identity Identity) error {
	components := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	current := string(filepath.Separator)
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
		current = filepath.Join(current, component)
		var stat unix.Stat_t
		if err := unix.Lstat(current, &stat); err != nil {
			return fmt.Errorf("inspect lexical path component %q: %w", current, err)
		}
		if stat.Mode&unix.S_IFMT == unix.S_IFLNK {
			if stat.Uid != 0 && stat.Uid != identity.UID {
				return fmt.Errorf("symbolic link %q has an unrelated owner", current)
			}
			continue
		}
		if remaining > 0 {
			if err := validateAncestor(stat, identity, current); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateAncestor(stat unix.Stat_t, identity Identity, path string) error {
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return fmt.Errorf("canonical ancestor %q is not a directory", path)
	}
	if stat.Uid != 0 && stat.Uid != identity.UID {
		return fmt.Errorf("canonical ancestor %q has an unrelated owner", path)
	}
	if !allows(identity, metadataFromStat(stat), true, AccessExecute) {
		return fmt.Errorf("canonical ancestor %q is not searchable by the intended identity", path)
	}
	if stat.Mode&0o022 != 0 && stat.Mode&unix.S_ISVTX == 0 {
		return fmt.Errorf("canonical ancestor %q is writable by group or other users without the sticky bit", path)
	}
	return nil
}

func validateTarget(metadata Metadata, policy Policy) error {
	label := policyLabel(policy)
	if metadata.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("%s must be a regular file", label)
	}
	if policy.RequireSingleLink && metadata.Links != 1 {
		return fmt.Errorf("%s must have exactly one hard link", label)
	}
	if metadata.UID != 0 && metadata.UID != policy.Identity.UID {
		return fmt.Errorf("%s has an unrelated owner", label)
	}
	if metadata.Mode&0o022 != 0 {
		return fmt.Errorf("%s is writable by group or other users", label)
	}
	if policy.RejectOtherPermissions && metadata.Mode&0o007 != 0 {
		return fmt.Errorf("%s has other-user permission bits", label)
	}
	if policy.MaxBytes > 0 && metadata.Size > policy.MaxBytes {
		return fmt.Errorf("%s is too large; maximum is %d bytes", label, policy.MaxBytes)
	}
	if !allows(policy.Identity, metadata, false, policy.Access) {
		return fmt.Errorf("%s does not grant required access to the intended identity", label)
	}
	return nil
}

func allows(identity Identity, metadata Metadata, directory bool, access Access) bool {
	mode := os.FileMode(metadata.Mode & 0o777)
	if identity.UID == 0 {
		if access != AccessExecute || directory {
			return true
		}
		return mode&0o111 != 0
	}
	var bits os.FileMode
	switch {
	case identity.UID == metadata.UID:
		bits = (mode >> 6) & 0o7
	default:
		if _, member := identity.Groups[metadata.GID]; member {
			bits = (mode >> 3) & 0o7
		} else {
			bits = mode & 0o7
		}
	}
	return bits&os.FileMode(access) != 0
}

func metadataFromStat(stat unix.Stat_t) Metadata {
	return Metadata{
		Device: uint64(stat.Dev),
		Inode:  stat.Ino,
		Links:  uint64(stat.Nlink),
		Mode:   stat.Mode,
		UID:    stat.Uid,
		GID:    stat.Gid,
		Size:   stat.Size,
	}
}

func (metadata Metadata) matches(stat unix.Stat_t) bool {
	return metadata == metadataFromStat(stat)
}

func cloneIdentity(identity Identity) Identity {
	cloned := Identity{UID: identity.UID, GID: identity.GID, Groups: make(map[uint32]struct{}, len(identity.Groups))}
	for group := range identity.Groups {
		cloned.Groups[group] = struct{}{}
	}
	return cloned
}

func clonePolicy(policy Policy) Policy {
	policy.Identity = cloneIdentity(policy.Identity)
	return policy
}

func policyLabel(policy Policy) string {
	if policy.Label == "" {
		return "file"
	}
	return policy.Label
}

func parseID(kind, value string) (uint32, error) {
	id, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("parse %s ID %q: %w", kind, value, err)
	}
	return uint32(id), nil
}
