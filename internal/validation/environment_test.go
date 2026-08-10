package validation

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/ezgamehost/ezdbbackup/internal/config"
	"github.com/ezgamehost/ezdbbackup/internal/dump"
	"github.com/ezgamehost/ezdbbackup/internal/jobresolve"
)

func TestReportAggregatesFindingsAndDetectsErrors(t *testing.T) {
	warning := Finding{Severity: SeverityWarning, Check: "configuration", Message: "plain HTTP"}
	cause := errors.New("permission denied")
	report := Report{}.Append(warning).Append(Finding{
		Severity: SeverityError,
		Job:      "production",
		Check:    "temp_directory",
		Message:  "temporary directory is not writable",
		Cause:    cause,
	})

	if len(report.Findings) != 2 {
		t.Fatalf("len(Findings) = %d, want 2", len(report.Findings))
	}
	if !report.HasErrors() {
		t.Fatal("HasErrors() = false, want true")
	}
	if !errors.Is(report.Findings[1], cause) {
		t.Fatalf("errors.Is(finding, cause) = false")
	}
	if (Report{}.Append(warning)).HasErrors() {
		t.Fatal("warning-only report HasErrors() = true, want false")
	}
}

func TestOSEnvironmentExecutableRequiresAbsoluteRegularExecutableAndVersion(t *testing.T) {
	requireLinux(t)
	runAs := currentUsername(t)
	env := OSEnvironment{}
	dir := secureValidationDir(t)
	executable := filepath.Join(dir, "mysqldump")
	writeExecutable(t, executable, "#!/bin/sh\n[ \"$1\" = --version ]\n")

	if err := env.CheckExecutableAs(context.Background(), executable, runAs); err != nil {
		t.Fatalf("CheckExecutable(valid) error = %v", err)
	}
	if err := env.CheckExecutableAs(context.Background(), "mysqldump", runAs); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("CheckExecutable(relative) error = %v, want absolute-path error", err)
	}
	if err := env.CheckExecutableAs(context.Background(), dir, runAs); err == nil || !strings.Contains(err.Error(), "regular") {
		t.Fatalf("CheckExecutable(directory) error = %v, want regular-file error", err)
	}
	missing := filepath.Join(dir, "missing")
	if err := env.CheckExecutableAs(context.Background(), missing, runAs); err == nil {
		t.Fatal("CheckExecutable(missing) error = nil")
	}

	nonExecutable := filepath.Join(dir, "not-executable")
	if err := os.WriteFile(nonExecutable, []byte("not executable"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := env.CheckExecutableAs(context.Background(), nonExecutable, runAs); err == nil || !strings.Contains(err.Error(), "executable") {
		t.Fatalf("CheckExecutable(non-executable) error = %v, want executable error", err)
	}

	badVersion := filepath.Join(dir, "bad-version")
	writeExecutable(t, badVersion, "#!/bin/sh\nexit 7\n")
	if err := env.CheckExecutableAs(context.Background(), badVersion, runAs); err == nil || !strings.Contains(err.Error(), "--version") {
		t.Fatalf("CheckExecutable(bad version) error = %v, want --version error", err)
	}
}

func TestOSEnvironmentRuntimeExecutableUsesEzdbbackupVersionCommand(t *testing.T) {
	requireLinux(t)
	dir := secureValidationDir(t)
	path := filepath.Join(dir, "ezdbbackup")
	writeExecutable(t, path, "#!/bin/sh\n[ \"$1\" = version ]\n")
	if err := (OSEnvironment{}).CheckRuntimeExecutable(context.Background(), path, currentUsername(t)); err != nil {
		t.Fatalf("CheckRuntimeExecutable(version subcommand) error = %v", err)
	}
}

func TestOSEnvironmentExecutableUsesIntendedUserPermissions(t *testing.T) {
	requireLinux(t)
	other := lookupDifferentUser(t)
	path := filepath.Join(secureValidationDir(t), "owner-only")
	writeExecutable(t, path, "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}

	err := (OSEnvironment{}).CheckExecutableAs(context.Background(), path, other.Username)
	if err == nil || !strings.Contains(err.Error(), other.Username) {
		t.Fatalf("CheckExecutable(owner-only as %q) error = %v, want intended-user permission error", other.Username, err)
	}
}

func TestOSEnvironmentExecutableRejectsUnrelatedOwner(t *testing.T) {
	requireLinux(t)
	other := lookupDifferentUser(t)
	dir := secureValidationDir(t)
	for _, directory := range []string{filepath.Dir(dir), dir} {
		if err := os.Chmod(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "public-but-untrusted")
	writeExecutable(t, path, "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}

	err := (OSEnvironment{}).CheckExecutableAs(context.Background(), path, other.Username)
	if err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("CheckExecutableAs(unrelated owner) error = %v, want ownership rejection", err)
	}
}

func TestVersionProbeCredentialsNeverRunAnotherUsersCodeAsInvoker(t *testing.T) {
	identity := userIdentity{
		uid: 2000,
		gid: 3000,
		groups: map[uint32]struct{}{
			3000: {},
			4000: {},
		},
	}
	credential, err := versionProbeCredential(identity, 0)
	if err != nil {
		t.Fatal(err)
	}
	if credential == nil || credential.Uid != identity.uid || credential.Gid != identity.gid {
		t.Fatalf("root probe credential = %#v, want uid %d gid %d", credential, identity.uid, identity.gid)
	}
	if _, err := versionProbeCredential(identity, 1000); err == nil || !strings.Contains(err.Error(), "safely") {
		t.Fatalf("unprivileged cross-user credential error = %v, want fail-closed rejection", err)
	}
	credential, err = versionProbeCredential(identity, identity.uid)
	if err != nil || credential != nil {
		t.Fatalf("same-user probe credential = %#v, %v; want ambient identity", credential, err)
	}
}

func TestOSEnvironmentRootRunsVersionProbeAsIntendedUser(t *testing.T) {
	requireLinux(t)
	if os.Geteuid() != 0 {
		t.Skip("credential transition fixture requires root")
	}
	other := lookupDifferentUser(t)
	dir := secureValidationDir(t)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "owned-by-run-as")
	writeExecutable(t, path, "#!/bin/sh\n[ \"$(id -u)\" = \"$EXPECTED_UID\" ]\n")
	uid, err := strconv.Atoi(other.Uid)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := strconv.Atoi(other.Gid)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(path, uid, gid); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}

	// The fixed probe environment intentionally omits EXPECTED_UID, so this
	// script would fail if it inherited the privileged validator environment.
	t.Setenv("EXPECTED_UID", other.Uid)
	if err := (OSEnvironment{}).CheckExecutableAs(context.Background(), path, other.Username); err == nil {
		t.Fatal("CheckExecutableAs() inherited the validator environment")
	}
	writeExecutable(t, path, "#!/bin/sh\n[ \"$(id -u)\" = \""+other.Uid+"\" ]\n")
	if err := os.Chown(path, uid, gid); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := (OSEnvironment{}).CheckExecutableAs(context.Background(), path, other.Username); err != nil {
		t.Fatalf("CheckExecutableAs(run_as-owned as root) error = %v", err)
	}
}

func TestOSEnvironmentAllowsSecureSymlinkTargets(t *testing.T) {
	requireLinux(t)
	runAs := currentUsername(t)
	env := OSEnvironment{}
	dir := secureValidationDir(t)

	executableTarget := filepath.Join(dir, "real-mysqldump")
	writeExecutable(t, executableTarget, "#!/bin/sh\nexit 0\n")
	executableLink := filepath.Join(dir, "mysqldump")
	if err := os.Symlink(executableTarget, executableLink); err != nil {
		t.Fatal(err)
	}
	if err := env.CheckExecutableAs(context.Background(), executableLink, runAs); err != nil {
		t.Fatalf("CheckExecutableAs(secure symlink) error = %v", err)
	}

	secretTarget := filepath.Join(dir, "real-secret")
	if err := os.WriteFile(secretTarget, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	secretLink := filepath.Join(dir, "secret")
	if err := os.Symlink(secretTarget, secretLink); err != nil {
		t.Fatal(err)
	}
	if err := env.CheckSecretFile(secretLink, runAs); err != nil {
		t.Fatalf("CheckSecretFile(secure symlink) error = %v", err)
	}

	directoryTarget := secureValidationDir(t)
	if err := os.Chmod(directoryTarget, 0o750); err != nil {
		t.Fatal(err)
	}
	directoryLink := filepath.Join(dir, "writable")
	if err := os.Symlink(directoryTarget, directoryLink); err != nil {
		t.Fatal(err)
	}
	if err := env.CheckWritableTarget(directoryLink, runAs); err != nil {
		t.Fatalf("CheckWritableTarget(secure symlink) error = %v", err)
	}
	if err := env.CheckLoggingTarget(directoryLink, []string{runAs}); err != nil {
		t.Fatalf("CheckLoggingTarget(secure symlink) error = %v", err)
	}
}

func TestOSEnvironmentAllowsSecureSymlinkedParentComponents(t *testing.T) {
	requireLinux(t)
	runAs := currentUsername(t)
	env := OSEnvironment{}
	root := secureValidationDir(t)
	realParent := filepath.Join(root, "real-parent")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	linkedParent := filepath.Join(root, "linked-parent")
	if err := os.Symlink(realParent, linkedParent); err != nil {
		t.Fatal(err)
	}

	executable := filepath.Join(realParent, "mysqldump")
	writeExecutable(t, executable, "#!/bin/sh\nexit 0\n")
	if err := env.CheckExecutableAs(context.Background(), filepath.Join(linkedParent, "mysqldump"), runAs); err != nil {
		t.Fatalf("CheckExecutableAs(secure symlinked parent) error = %v", err)
	}

	secret := filepath.Join(realParent, "secret")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := env.CheckSecretFile(filepath.Join(linkedParent, "secret"), runAs); err != nil {
		t.Fatalf("CheckSecretFile(secure symlinked parent) error = %v", err)
	}

	existingDirectory := filepath.Join(realParent, "existing")
	if err := os.Mkdir(existingDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := env.CheckWritableTarget(filepath.Join(linkedParent, "existing"), runAs); err != nil {
		t.Fatalf("CheckWritableTarget(existing under secure symlinked parent) error = %v", err)
	}

	if err := env.CheckWritableTarget(filepath.Join(linkedParent, "missing", "target"), runAs); err != nil {
		t.Fatalf("CheckWritableTarget(missing under secure symlinked parent) error = %v", err)
	}
}

func TestOSEnvironmentRejectsSymlinkBeforeDotDotComponent(t *testing.T) {
	requireLinux(t)
	runAs := currentUsername(t)
	root := secureValidationDir(t)
	realParent := filepath.Join(root, "real-parent")
	child := filepath.Join(realParent, "child")
	if err := os.MkdirAll(child, 0o700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(realParent, "mysqldump")
	writeExecutable(t, executable, "#!/bin/sh\nexit 0\n")
	linkedParent := filepath.Join(root, "linked-parent")
	if err := os.Symlink(child, linkedParent); err != nil {
		t.Fatal(err)
	}

	path := linkedParent + string(filepath.Separator) + ".." + string(filepath.Separator) + "mysqldump"
	err := (OSEnvironment{}).CheckExecutableAs(context.Background(), path, runAs)
	if err == nil || !strings.Contains(err.Error(), "clean absolute path") {
		t.Fatalf("CheckExecutableAs(symlink/..) error = %v, want clean-path error", err)
	}
}

func TestOSEnvironmentRequiresIntendedUserToTraverseExecutablePath(t *testing.T) {
	requireLinux(t)
	other := lookupDifferentUser(t)
	locked := secureValidationDir(t)
	if err := os.Chmod(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(locked, "public-executable")
	writeExecutable(t, path, "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}

	err := (OSEnvironment{}).CheckExecutableAs(context.Background(), path, other.Username)
	if err == nil || !strings.Contains(err.Error(), "traverse") {
		t.Fatalf("CheckExecutableAs(untraversable parent) error = %v, want traversal error", err)
	}
}

func TestTraversalRejectsReplaceableParentButAllowsStickyBoundary(t *testing.T) {
	requireLinux(t)
	runAs := currentUsername(t)
	identity, err := lookupIdentity(runAs)
	if err != nil {
		t.Fatal(err)
	}
	root := secureValidationDir(t)
	replaceable := filepath.Join(root, "replaceable")
	if err := os.Mkdir(replaceable, 0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(replaceable, 0o770); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(replaceable, "target")

	if err := identity.checkParentTraversal(target); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("checkParentTraversal(non-sticky shared parent) error = %v, want replaceability rejection", err)
	}
	if err := os.Chmod(replaceable, os.ModeSticky|0o770); err != nil {
		t.Fatal(err)
	}
	if err := identity.checkParentTraversal(target); err != nil {
		t.Fatalf("checkParentTraversal(sticky shared parent) error = %v", err)
	}
}

func TestOSEnvironmentWritableTargetUsesExistingDirectoryOrNearestParent(t *testing.T) {
	requireLinux(t)
	runAs := currentUsername(t)
	env := OSEnvironment{}
	writable := secureValidationDir(t)
	if err := env.CheckWritableTarget(writable, runAs); err != nil {
		t.Fatalf("CheckWritableTarget(existing) error = %v", err)
	}

	missing := filepath.Join(writable, "one", "two")
	if err := env.CheckWritableTarget(missing, runAs); err != nil {
		t.Fatalf("CheckWritableTarget(missing with writable parent) error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(writable, "one")); !os.IsNotExist(err) {
		t.Fatalf("validation created directory or returned unexpected stat error: %v", err)
	}

	other := lookupDifferentUser(t)
	locked := secureValidationDir(t)
	if err := os.Chmod(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	err := env.CheckWritableTarget(filepath.Join(locked, "missing"), other.Username)
	if err == nil || !strings.Contains(err.Error(), "nearest existing parent") {
		t.Fatalf("CheckWritableTarget(unwritable parent) error = %v, want nearest-parent error", err)
	}

	file := filepath.Join(writable, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := env.CheckWritableTarget(file, runAs); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("CheckWritableTarget(file) error = %v, want directory error", err)
	}
}

// This fails if a shared non-sticky temporary directory permits another user
// to rename or replace a staged artifact entry.
func TestOSEnvironmentStagingTargetRejectsSharedNonStickyDirectory(t *testing.T) {
	requireLinux(t)
	runAs := currentUsername(t)
	directory := secureValidationDir(t)
	if err := os.Chmod(directory, 0o777); err != nil {
		t.Fatal(err)
	}

	err := (OSEnvironment{}).CheckStagingTarget(directory, runAs)
	if err == nil || !strings.Contains(err.Error(), "sticky") {
		t.Fatalf("CheckStagingTarget(shared non-sticky) error = %v, want sticky-directory safety error", err)
	}
}

// This fails if a secure sticky parent such as /tmp is rejected even though
// Stage creates a private mode-0700 work directory beneath it.
func TestOSEnvironmentStagingTargetAllowsStickyAndPrivateDirectories(t *testing.T) {
	requireLinux(t)
	runAs := currentUsername(t)
	env := OSEnvironment{}

	privateDirectory := secureValidationDir(t)
	if err := os.Chmod(privateDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := env.CheckStagingTarget(privateDirectory, runAs); err != nil {
		t.Fatalf("CheckStagingTarget(private) error = %v", err)
	}

	stickyParent := secureValidationDir(t)
	if err := os.Chmod(stickyParent, os.ModeSticky|0o777); err != nil {
		t.Fatal(err)
	}
	missingTarget := filepath.Join(stickyParent, "ezdbbackup")
	if err := env.CheckStagingTarget(missingTarget, runAs); err != nil {
		t.Fatalf("CheckStagingTarget(sticky parent) error = %v", err)
	}
	if _, err := os.Stat(missingTarget); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("CheckStagingTarget created target or returned unexpected stat error: %v", err)
	}
}

// This fails if a private target can be renamed wholesale by a writer of its
// containing directory, stranding the sensitive work directory after Stage.
func TestOSEnvironmentStagingTargetRequiresSafeAncestors(t *testing.T) {
	requireLinux(t)
	runAs := currentUsername(t)
	sharedParent := secureValidationDir(t)
	if err := os.Chmod(sharedParent, 0o777); err != nil {
		t.Fatal(err)
	}
	privateTarget := filepath.Join(sharedParent, "staging")
	if err := os.Mkdir(privateTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := (OSEnvironment{}).CheckStagingTarget(privateTarget, runAs); err == nil {
		t.Fatal("CheckStagingTarget(private beneath shared non-sticky parent) error = nil")
	}
	if err := os.Chmod(sharedParent, os.ModeSticky|0o777); err != nil {
		t.Fatal(err)
	}
	if err := (OSEnvironment{}).CheckStagingTarget(privateTarget, runAs); err != nil {
		t.Fatalf("CheckStagingTarget(private beneath sticky parent) error = %v", err)
	}
}

// This fails if an intended root process accepts a temporary location owned
// by an unrelated account whose owner can later replace directory entries.
func TestOSEnvironmentStagingTargetRejectsAttackerOwnedDirectory(t *testing.T) {
	requireLinux(t)
	if os.Geteuid() != 0 {
		t.Skip("ownership fixture requires root")
	}
	other := lookupDifferentUser(t)
	directory := secureValidationDir(t)
	uid, err := strconv.Atoi(other.Uid)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := strconv.Atoi(other.Gid)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(directory, uid, gid); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, os.ModeSticky|0o777); err != nil {
		t.Fatal(err)
	}

	err = (OSEnvironment{}).CheckStagingTarget(directory, currentUsername(t))
	if err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("CheckStagingTarget(attacker-owned) error = %v, want owner safety error", err)
	}
}

func TestOSEnvironmentSecretFileRules(t *testing.T) {
	requireLinux(t)
	runAs := currentUsername(t)
	env := OSEnvironment{}
	path := filepath.Join(secureValidationDir(t), "secret")
	if err := os.WriteFile(path, []byte("secret"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := env.CheckSecretFile(path, runAs); err != nil {
		t.Fatalf("CheckSecretFile(valid) error = %v", err)
	}
	if err := env.CheckSecretFile("relative", runAs); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("CheckSecretFile(relative) error = %v, want absolute error", err)
	}
	if err := env.CheckSecretFile(secureValidationDir(t), runAs); err == nil || !strings.Contains(err.Error(), "regular") {
		t.Fatalf("CheckSecretFile(directory) error = %v, want regular-file error", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := env.CheckSecretFile(path, runAs); err == nil || !strings.Contains(err.Error(), "other-user") {
		t.Fatalf("CheckSecretFile(other bits) error = %v, want other-user error", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	other := lookupDifferentUser(t)
	if err := env.CheckSecretFile(path, other.Username); err == nil || !strings.Contains(err.Error(), "readable") {
		t.Fatalf("CheckSecretFile(unreadable as %q) error = %v, want readable error", other.Username, err)
	}
}

func TestOSEnvironmentConfigFileRequiresScheduledReadabilityAndSecrecy(t *testing.T) {
	requireLinux(t)
	env := OSEnvironment{}
	runAs := currentUsername(t)
	dir := secureValidationDir(t)
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte("version: 1\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := env.CheckConfigFile(path, runAs); err != nil {
		t.Fatalf("CheckConfigFile(valid) error = %v", err)
	}

	if err := os.Chmod(path, 0o642); err != nil {
		t.Fatal(err)
	}
	if err := env.CheckConfigFile(path, runAs); err == nil || !strings.Contains(err.Error(), "other") {
		t.Fatalf("CheckConfigFile(other-readable) error = %v", err)
	}
	if err := os.Chmod(path, 0o660); err != nil {
		t.Fatal(err)
	}
	if err := env.CheckConfigFile(path, runAs); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("CheckConfigFile(group-writable) error = %v", err)
	}

	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	other := lookupDifferentUser(t)
	if err := env.CheckConfigFile(path, other.Username); err == nil || !strings.Contains(err.Error(), "readable") {
		t.Fatalf("CheckConfigFile(root/current 0600 as %q) error = %v, want scheduled readability failure", other.Username, err)
	}
}

func TestOSEnvironmentConfigFileAllowsSecureSymlink(t *testing.T) {
	requireLinux(t)
	dir := secureValidationDir(t)
	target := filepath.Join(dir, "config-target.yml")
	if err := os.WriteFile(target, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "config.yml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := (OSEnvironment{}).CheckConfigFile(link, currentUsername(t)); err != nil {
		t.Fatalf("CheckConfigFile(secure symlink) error = %v", err)
	}
}

// Reopening source.RequestedPath here would inspect the second file rather
// than the descriptor-backed source whose bytes were actually decoded.
func TestOSEnvironmentChecksExactLoadedConfigurationSource(t *testing.T) {
	requireLinux(t)
	dir := secureValidationDir(t)
	first := filepath.Join(dir, "config-first.yml")
	second := filepath.Join(dir, "config-second.yml")
	for _, target := range []string{first, second} {
		if err := os.WriteFile(target, []byte("version: 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(dir, "config.yml")
	if err := os.Symlink(first, link); err != nil {
		t.Fatal(err)
	}
	cfg, findings := config.Load(link)
	if findings.HasErrors() {
		t.Fatalf("Load() findings = %v", findings)
	}
	source, ok := cfg.Source()
	if !ok {
		t.Fatal("loaded configuration has no source")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, link); err != nil {
		t.Fatal(err)
	}

	env := OSEnvironment{}
	if err := env.CheckConfigSource(source, currentUsername(t)); err != nil {
		t.Fatalf("CheckConfigSource(exact unchanged source) error = %v", err)
	}
	if err := os.Rename(first, first+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first, []byte("version: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := env.CheckConfigSource(source, currentUsername(t)); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("CheckConfigSource(replaced source) error = %v, want identity rejection", err)
	}
}

func TestOSEnvironmentRejectsSymlinkRepointedDuringInspection(t *testing.T) {
	requireLinux(t)
	dir := secureValidationDir(t)
	first := filepath.Join(dir, "config-first.yml")
	second := filepath.Join(dir, "config-second.yml")
	for _, target := range []string{first, second} {
		if err := os.WriteFile(target, []byte("version: 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(dir, "config.yml")
	if err := os.Symlink(first, link); err != nil {
		t.Fatal(err)
	}
	env := OSEnvironment{beforePathRecheck: func(string) {
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(second, link); err != nil {
			t.Fatal(err)
		}
	}}

	err := env.CheckConfigFile(link, currentUsername(t))
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("CheckConfigFile(repointed symlink) error = %v, want identity rejection", err)
	}
}

// An earlier CheckSecretFile result must not authorize a later plain pathname
// read after a sticky-directory symlink has been substituted.
func TestValidatedSecretSymlinkSubstitutionIsRejectedAtResolution(t *testing.T) {
	requireLinux(t)
	root := secureValidationDir(t)
	shared := filepath.Join(root, "shared")
	if err := os.Mkdir(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, os.ModeSticky|0o777); err != nil {
		t.Fatal(err)
	}
	trusted := filepath.Join(shared, "trusted-secret")
	attacker := filepath.Join(shared, "attacker-secret")
	if err := os.WriteFile(trusted, []byte("trusted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(attacker, []byte("attacker\n"), 0o660); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(shared, "mysql-secret")
	if err := os.Symlink(trusted, link); err != nil {
		t.Fatal(err)
	}
	runAs := currentUsername(t)
	if err := (OSEnvironment{}).CheckSecretFile(link, runAs); err != nil {
		t.Fatalf("initial CheckSecretFile() error = %v", err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(attacker, link); err != nil {
		t.Fatal(err)
	}
	request, err := (jobresolve.Resolver{}).Dump(config.JobConfig{
		RunAs: runAs,
		MySQL: config.MySQLConfig{PasswordFile: link},
	})
	if err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("Dump(swapped secret) = %#v, error = %v; want point-of-use rejection", request, err)
	}
}

func TestOSEnvironmentRejectsWritableExecutableAndPathSwap(t *testing.T) {
	requireLinux(t)
	dir := secureValidationDir(t)
	path := filepath.Join(dir, "ezdbbackup")
	writeExecutable(t, path, "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(path, 0o770); err != nil {
		t.Fatal(err)
	}
	if err := (OSEnvironment{}).CheckRuntimeExecutable(context.Background(), path, currentUsername(t)); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("CheckRuntimeExecutable(group-writable) error = %v", err)
	}

	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	env := OSEnvironment{beforePathRecheck: func(resolved string) {
		if err := os.Rename(resolved, resolved+".original"); err != nil {
			t.Fatal(err)
		}
		writeExecutable(t, resolved, "#!/bin/sh\nexit 0\n")
	}}
	if err := env.CheckRuntimeExecutable(context.Background(), path, currentUsername(t)); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("CheckRuntimeExecutable(swapped) error = %v, want identity rejection", err)
	}
}

func TestOSEnvironmentSameUIDVersionProbeRejectsRetainedPrivilege(t *testing.T) {
	requireLinux(t)
	if os.Geteuid() == 0 {
		t.Skip("root run_as may intentionally retain root privileges")
	}
	directory := secureValidationDir(t)
	marker := filepath.Join(directory, "probe-ran")
	path := filepath.Join(directory, "mysqldump")
	writeExecutable(t, path, "#!/bin/sh\n: > "+strconv.Quote(marker)+"\n")
	state, err := currentProcessCredentialState()
	if err != nil {
		t.Fatal(err)
	}
	state.capPermitted = 1
	env := OSEnvironment{readCredentialState: func() (processCredentialState, error) {
		return state, nil
	}}
	if err := env.CheckExecutableAs(context.Background(), path, currentUsername(t)); err == nil || !strings.Contains(err.Error(), "capability") {
		t.Fatalf("CheckExecutableAs(retained capability) error = %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("version probe marker stat error = %v, want configured code not executed", err)
	}
}

// When run as root this directly exercises both a root job and a cross-user
// job through an attacker-owned symlink in a sticky directory. The substitute
// executable must never run after the earlier validation succeeds.
func TestValidatedExecutableStickySymlinkSwapNeverRunsUnrelatedOwner(t *testing.T) {
	requireLinux(t)
	if os.Geteuid() != 0 {
		t.Skip("requires root to construct distinct executable owners and credentials")
	}
	rootAccount, err := user.LookupId("0")
	if err != nil {
		t.Fatal(err)
	}
	nonRoot := lookupDistinctNonRootUsers(t, 2)
	tests := []struct {
		name     string
		runAs    *user.User
		attacker *user.User
	}{
		{name: "root job", runAs: rootAccount, attacker: nonRoot[0]},
		{name: "cross user job", runAs: nonRoot[0], attacker: nonRoot[1]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			directory, err := os.MkdirTemp("/tmp", "ezdbbackup-exec-swap-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(directory) })
			if err := os.Chmod(directory, os.ModeSticky|0o777); err != nil {
				t.Fatal(err)
			}
			trusted := filepath.Join(directory, "trusted")
			attacker := filepath.Join(directory, "attacker")
			marker := filepath.Join(directory, "attacker-ran")
			writeOwnedExecutable(t, trusted, "#!/bin/sh\nexit 0\n", tt.runAs)
			writeOwnedExecutable(t, attacker, "#!/bin/sh\n: > \""+marker+"\"\n", tt.attacker)
			link := filepath.Join(directory, "mysqldump")
			if err := os.Symlink(trusted, link); err != nil {
				t.Fatal(err)
			}
			runAsUID, _ := strconv.Atoi(tt.runAs.Uid)
			runAsGID, _ := strconv.Atoi(tt.runAs.Gid)
			if err := os.Lchown(link, runAsUID, runAsGID); err != nil {
				t.Fatal(err)
			}
			if err := (OSEnvironment{}).CheckExecutableAs(context.Background(), link, tt.runAs.Username); err != nil {
				t.Fatalf("initial executable validation error = %v", err)
			}
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(attacker, link); err != nil {
				t.Fatal(err)
			}
			attackerUID, _ := strconv.Atoi(tt.attacker.Uid)
			attackerGID, _ := strconv.Atoi(tt.attacker.Gid)
			if err := os.Lchown(link, attackerUID, attackerGID); err != nil {
				t.Fatal(err)
			}

			err = (dump.ExecRunner{}).Run(context.Background(), dump.Request{
				Binary: link,
				RunAs:  tt.runAs.Username,
			}, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), "unrelated owner") {
				t.Fatalf("Run(swapped executable) error = %v, want unrelated-owner rejection", err)
			}
			if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("attacker marker stat error = %v, want attacker never executed", statErr)
			}
		})
	}
}

func TestResolvedSecretStickySymlinkSwapRejectsUnrelatedOwner(t *testing.T) {
	requireLinux(t)
	if os.Geteuid() != 0 {
		t.Skip("requires root to construct distinct secret owners")
	}
	rootAccount, err := user.LookupId("0")
	if err != nil {
		t.Fatal(err)
	}
	nonRoot := lookupDistinctNonRootUsers(t, 2)
	for _, tt := range []struct {
		name     string
		runAs    *user.User
		attacker *user.User
	}{
		{name: "root job", runAs: rootAccount, attacker: nonRoot[0]},
		{name: "cross user job", runAs: nonRoot[0], attacker: nonRoot[1]},
	} {
		t.Run(tt.name, func(t *testing.T) {
			directory, err := os.MkdirTemp("/tmp", "ezdbbackup-secret-swap-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(directory) })
			if err := os.Chmod(directory, os.ModeSticky|0o777); err != nil {
				t.Fatal(err)
			}
			trusted := filepath.Join(directory, "trusted")
			attacker := filepath.Join(directory, "attacker")
			writeOwnedFile(t, trusted, "trusted\n", 0o600, tt.runAs)
			writeOwnedFile(t, attacker, "attacker\n", 0o600, tt.attacker)
			link := filepath.Join(directory, "secret")
			if err := os.Symlink(trusted, link); err != nil {
				t.Fatal(err)
			}
			if err := (OSEnvironment{}).CheckSecretFile(link, tt.runAs.Username); err != nil {
				t.Fatalf("initial CheckSecretFile() error = %v", err)
			}
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(attacker, link); err != nil {
				t.Fatal(err)
			}
			attackerUID, _ := strconv.Atoi(tt.attacker.Uid)
			attackerGID, _ := strconv.Atoi(tt.attacker.Gid)
			if err := os.Lchown(link, attackerUID, attackerGID); err != nil {
				t.Fatal(err)
			}
			_, err = (jobresolve.Resolver{}).Dump(config.JobConfig{
				RunAs: tt.runAs.Username,
				MySQL: config.MySQLConfig{PasswordFile: link},
			})
			if err == nil || !strings.Contains(err.Error(), "unrelated owner") {
				t.Fatalf("Dump(swapped unrelated secret) error = %v", err)
			}
		})
	}
}

func writeOwnedExecutable(t *testing.T, path, contents string, owner *user.User) {
	t.Helper()
	writeOwnedFile(t, path, contents, 0o700, owner)
}

func writeOwnedFile(t *testing.T, path, contents string, mode os.FileMode, owner *user.User) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
	uid, err := strconv.Atoi(owner.Uid)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := strconv.Atoi(owner.Gid)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(path, uid, gid); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func lookupDistinctNonRootUsers(t *testing.T, count int) []*user.User {
	t.Helper()
	var accounts []*user.User
	seen := map[string]bool{"0": true}
	for _, name := range []string{"nobody", "daemon", "www-data", "bin", "sys"} {
		account, err := user.Lookup(name)
		if err != nil || seen[account.Uid] {
			continue
		}
		seen[account.Uid] = true
		accounts = append(accounts, account)
		if len(accounts) == count {
			return accounts
		}
	}
	t.Skipf("need %d distinct non-root local users", count)
	return nil
}

func TestLogDirectoryPolicySameUserSharedGroupAndIncompatibleUsers(t *testing.T) {
	tests := []struct {
		name       string
		stat       syscall.Stat_t
		identities []userIdentity
		wantErr    bool
	}{
		{
			name:       "same user private",
			stat:       syscall.Stat_t{Uid: 1000, Gid: 1000, Mode: syscall.S_IFDIR | 0o750},
			identities: []userIdentity{{uid: 1000, groups: map[uint32]struct{}{1000: {}}}},
		},
		{
			name: "shared setgid group",
			stat: syscall.Stat_t{Uid: 0, Gid: 2000, Mode: syscall.S_IFDIR | syscall.S_ISGID | 0o770},
			identities: []userIdentity{
				{uid: 1000, groups: map[uint32]struct{}{2000: {}}},
				{uid: 1001, groups: map[uint32]struct{}{2000: {}}},
			},
		},
		{
			name: "sticky shared group cannot rotate cross user",
			stat: syscall.Stat_t{Uid: 0, Gid: 2000, Mode: syscall.S_IFDIR | syscall.S_ISGID | syscall.S_ISVTX | 0o770},
			identities: []userIdentity{
				{uid: 1000, groups: map[uint32]struct{}{2000: {}}},
				{uid: 1001, groups: map[uint32]struct{}{2000: {}}},
			},
			wantErr: true,
		},
		{
			name:       "sticky group writable directory is shared even for one configured user",
			stat:       syscall.Stat_t{Uid: 1000, Gid: 2000, Mode: syscall.S_IFDIR | syscall.S_ISGID | syscall.S_ISVTX | 0o770},
			identities: []userIdentity{{uid: 1000, groups: map[uint32]struct{}{2000: {}}}},
			wantErr:    true,
		},
		{
			name:       "private owner sticky directory remains mutable by owner",
			stat:       syscall.Stat_t{Uid: 1000, Gid: 1000, Mode: syscall.S_IFDIR | syscall.S_ISVTX | 0o700},
			identities: []userIdentity{{uid: 1000, groups: map[uint32]struct{}{1000: {}}}},
		},
		{
			name: "root and nonroot shared setgid group",
			stat: syscall.Stat_t{Uid: 0, Gid: 2000, Mode: syscall.S_IFDIR | syscall.S_ISGID | 0o770},
			identities: []userIdentity{
				{uid: 0, groups: map[uint32]struct{}{0: {}}},
				{uid: 1000, groups: map[uint32]struct{}{2000: {}}},
			},
		},
		{
			name: "incompatible groups",
			stat: syscall.Stat_t{Uid: 0, Gid: 2000, Mode: syscall.S_IFDIR | syscall.S_ISGID | 0o770},
			identities: []userIdentity{
				{uid: 1000, groups: map[uint32]struct{}{2000: {}}},
				{uid: 1001, groups: map[uint32]struct{}{3000: {}}},
			},
			wantErr: true,
		},
		{
			name: "shared without setgid",
			stat: syscall.Stat_t{Uid: 0, Gid: 2000, Mode: syscall.S_IFDIR | 0o770},
			identities: []userIdentity{
				{uid: 1000, groups: map[uint32]struct{}{2000: {}}},
				{uid: 1001, groups: map[uint32]struct{}{2000: {}}},
			},
			wantErr: true,
		},
		{
			name:       "same user malformed shared permissions",
			stat:       syscall.Stat_t{Uid: 1000, Gid: 2000, Mode: syscall.S_IFDIR | syscall.S_ISGID | 0o720},
			identities: []userIdentity{{uid: 1000, groups: map[uint32]struct{}{2000: {}}}},
			wantErr:    true,
		},
		{
			name:       "same user shared with unrelated group",
			stat:       syscall.Stat_t{Uid: 1000, Gid: 2000, Mode: syscall.S_IFDIR | syscall.S_ISGID | 0o770},
			identities: []userIdentity{{uid: 1000, groups: map[uint32]struct{}{3000: {}}}},
			wantErr:    true,
		},
		{
			name: "same UID aliases preserve distinct group sets",
			stat: syscall.Stat_t{Uid: 1000, Gid: 2000, Mode: syscall.S_IFDIR | syscall.S_ISGID | 0o770},
			identities: []userIdentity{
				{uid: 1000, groups: map[uint32]struct{}{2000: {}}},
				{uid: 1000, groups: map[uint32]struct{}{3000: {}}},
			},
			wantErr: true,
		},
		{
			name:       "same user private read group is unrelated",
			stat:       syscall.Stat_t{Uid: 1000, Gid: 2000, Mode: syscall.S_IFDIR | syscall.S_ISGID | 0o750},
			identities: []userIdentity{{uid: 1000, groups: map[uint32]struct{}{3000: {}}}},
			wantErr:    true,
		},
		{
			name: "multiple users require root owned trust boundary",
			stat: syscall.Stat_t{Uid: 1000, Gid: 2000, Mode: syscall.S_IFDIR | syscall.S_ISGID | 0o770},
			identities: []userIdentity{
				{uid: 1000, groups: map[uint32]struct{}{2000: {}}},
				{uid: 1001, groups: map[uint32]struct{}{2000: {}}},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateLogTargetDirectory(tt.stat, tt.identities)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateLogTargetDirectory() error = %v, wantErr %t", err, tt.wantErr)
			}
		})
	}
}

func TestLogDirectoryTraversalChecksLexicalAndResolvedPaths(t *testing.T) {
	requireLinux(t)
	root := secureValidationDir(t)
	realParent := filepath.Join(root, "real-parent")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	logDirectory := filepath.Join(realParent, "logs")
	if err := os.Mkdir(logDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	linkedParent := filepath.Join(root, "linked-parent")
	if err := os.Symlink(realParent, linkedParent); err != nil {
		t.Fatal(err)
	}

	identity := userIdentity{uid: uint32(os.Geteuid()) + 1, groups: map[uint32]struct{}{}}
	err := validateLogTargetTraversal(filepath.Join(linkedParent, "logs"), logDirectory, []userIdentity{identity})
	if err == nil || !strings.Contains(err.Error(), "lexical path traversal") {
		t.Fatalf("validateLogTargetTraversal(unsearchable lexical parent) error = %v, want traversal error", err)
	}
}

func TestMissingLogDirectoryRequiresNonreplaceableExistingParent(t *testing.T) {
	requireLinux(t)
	parent := secureValidationDir(t)
	if err := os.Chmod(parent, 0o770); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(parent, "logs")
	env := OSEnvironment{}
	if err := env.CheckLoggingTarget(target, []string{currentUsername(t)}); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("CheckLoggingTarget(non-sticky parent) error = %v, want replaceability rejection", err)
	}
	if err := os.Chmod(parent, os.ModeSticky|0o770); err != nil {
		t.Fatal(err)
	}
	if err := env.CheckLoggingTarget(target, []string{currentUsername(t)}); err != nil {
		t.Fatalf("CheckLoggingTarget(sticky parent) error = %v", err)
	}
}

func TestExistingLogDirectoryRequiresNonreplaceableLexicalAndResolvedParents(t *testing.T) {
	requireLinux(t)
	parent := secureValidationDir(t)
	target := filepath.Join(parent, "logs")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o770); err != nil {
		t.Fatal(err)
	}
	env := OSEnvironment{}
	if err := env.CheckLoggingTarget(target, []string{currentUsername(t)}); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("CheckLoggingTarget(existing beneath non-sticky parent) error = %v, want replaceability rejection", err)
	}
	if err := os.Chmod(parent, os.ModeSticky|0o770); err != nil {
		t.Fatal(err)
	}
	if err := env.CheckLoggingTarget(target, []string{currentUsername(t)}); err != nil {
		t.Fatalf("CheckLoggingTarget(existing beneath sticky parent) error = %v", err)
	}
}

func TestOSEnvironmentUserAndCronPathRules(t *testing.T) {
	env := OSEnvironment{}
	if err := env.CheckUser(currentUsername(t)); err != nil {
		t.Fatalf("CheckUser(current) error = %v", err)
	}
	if err := env.CheckUser("ezdbbackup-user-that-does-not-exist"); err == nil {
		t.Fatal("CheckUser(missing) error = nil")
	}
	if err := env.CheckRunIdentity(currentUsername(t)); err != nil {
		t.Fatalf("CheckRunIdentity(current) error = %v", err)
	}
	other := lookupDifferentUser(t)
	if err := env.CheckRunIdentity(other.Username); err == nil || !strings.Contains(err.Error(), "effective user") {
		t.Fatalf("CheckRunIdentity(%q) error = %v, want mismatch", other.Username, err)
	}

	for _, valid := range []string{"/usr/local/bin/ezdbbackup", "/etc/ezdbbackup/config.yml"} {
		if err := env.CheckCronPath(valid); err != nil {
			t.Fatalf("CheckCronPath(%q) error = %v", valid, err)
		}
	}
	for _, invalid := range []string{"relative", "/tmp/bad\npath", "/tmp/bad\rpath", "/tmp/bad\x00path", "/tmp/bad%path"} {
		if err := env.CheckCronPath(invalid); err == nil {
			t.Fatalf("CheckCronPath(%q) error = nil", invalid)
		}
	}
}

// Checking EUID alone would accept a process that retained a privileged
// primary group, supplementary group, or Linux capability across setuid.
func TestRunIdentityStateRequiresOrdinaryNonRootLoginCredentials(t *testing.T) {
	identity := userIdentity{
		uid: 1000,
		gid: 100,
		groups: map[uint32]struct{}{
			100: {},
			200: {},
		},
	}
	valid := processCredentialState{
		ruid:  1000,
		euid:  1000,
		suid:  1000,
		fsuid: 1000,
		rgid:  100,
		egid:  100,
		sgid:  100,
		fsgid: 100,
		groups: map[uint32]struct{}{
			100: {},
			200: {},
		},
	}
	tests := []struct {
		name  string
		state processCredentialState
		want  string
	}{
		{name: "valid ordinary login", state: valid},
		{name: "wrong effective uid", state: withCredentialState(valid, func(state *processCredentialState) { state.euid = 0 }), want: "effective user"},
		{name: "retained real uid", state: withCredentialState(valid, func(state *processCredentialState) { state.ruid = 0 }), want: "real user"},
		{name: "retained saved uid", state: withCredentialState(valid, func(state *processCredentialState) { state.suid = 0 }), want: "saved user"},
		{name: "retained filesystem uid", state: withCredentialState(valid, func(state *processCredentialState) { state.fsuid = 0 }), want: "filesystem user"},
		{name: "retained primary gid", state: withCredentialState(valid, func(state *processCredentialState) { state.egid = 0 }), want: "effective group"},
		{name: "retained real gid", state: withCredentialState(valid, func(state *processCredentialState) { state.rgid = 0 }), want: "real group"},
		{name: "retained saved gid", state: withCredentialState(valid, func(state *processCredentialState) { state.sgid = 0 }), want: "saved group"},
		{name: "retained filesystem gid", state: withCredentialState(valid, func(state *processCredentialState) { state.fsgid = 0 }), want: "filesystem group"},
		{name: "missing login group", state: withCredentialState(valid, func(state *processCredentialState) { delete(state.groups, 200) }), want: "supplementary"},
		{name: "retained extra group", state: withCredentialState(valid, func(state *processCredentialState) { state.groups[0] = struct{}{} }), want: "supplementary"},
		{name: "effective capability", state: withCredentialState(valid, func(state *processCredentialState) { state.capEffective = 1 }), want: "capabilit"},
		{name: "permitted capability", state: withCredentialState(valid, func(state *processCredentialState) { state.capPermitted = 1 }), want: "capabilit"},
		{name: "inheritable capability", state: withCredentialState(valid, func(state *processCredentialState) { state.capInheritable = 1 }), want: "capabilit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRunIdentityState(identity, tt.state)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("validateRunIdentityState() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), tt.want) {
				t.Fatalf("validateRunIdentityState() error = %v, want %q", err, tt.want)
			}
		})
	}

	if err := validateRunIdentityState(
		userIdentity{uid: 0, gid: 0, groups: map[uint32]struct{}{0: {}}},
		processCredentialState{euid: 0, egid: 123, groups: map[uint32]struct{}{456: {}}, capEffective: 1},
	); err != nil {
		t.Fatalf("root run_as retained privileges error = %v", err)
	}
}

const runIdentityHelperUserEnv = "EZDBBACKUP_TEST_RUN_IDENTITY_USER"

// Root-only execution directly crosses a complete syscall credential boundary
// and asks the child to validate its real UID/GID/group/capability state.
func TestOSEnvironmentRootCredentialTransitionMatchesOrdinaryLogin(t *testing.T) {
	requireLinux(t)
	if os.Getenv(runIdentityHelperUserEnv) != "" {
		return
	}
	if os.Geteuid() != 0 {
		t.Skip("requires root to exercise a real credential transition")
	}
	target := lookupDistinctNonRootUsers(t, 1)[0]
	identity, err := lookupIdentity(target.Username)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := versionProbeCredential(identity, 0)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := os.MkdirTemp("/tmp", "ezdbbackup-identity-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	currentBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binaryBytes, err := os.ReadFile(currentBinary)
	if err != nil {
		t.Fatal(err)
	}
	helperBinary := filepath.Join(directory, "identity-helper")
	if err := os.WriteFile(helperBinary, binaryBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	clear(binaryBytes)
	command := exec.Command(helperBinary, "-test.run=^TestOSEnvironmentRunIdentityCredentialHelper$")
	command.Env = append(os.Environ(), runIdentityHelperUserEnv+"="+target.Username)
	command.SysProcAttr = &syscall.SysProcAttr{Credential: credential}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("credential helper error = %v, output = %s", err, output)
	}
}

func TestOSEnvironmentRunIdentityCredentialHelper(t *testing.T) {
	name := os.Getenv(runIdentityHelperUserEnv)
	if name == "" {
		return
	}
	if err := (OSEnvironment{}).CheckRunIdentity(name); err != nil {
		t.Fatalf("CheckRunIdentity(%q) after credential transition error = %v", name, err)
	}
}

func withCredentialState(state processCredentialState, mutate func(*processCredentialState)) processCredentialState {
	cloned := state
	cloned.groups = make(map[uint32]struct{}, len(state.groups))
	for group := range state.groups {
		cloned.groups[group] = struct{}{}
	}
	mutate(&cloned)
	return cloned
}

func requireLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("ezdbbackup v1 supports Linux only")
	}
}

func secureValidationDir(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return directory
}

func currentUsername(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	return u.Username
}

func lookupDifferentUser(t *testing.T) *user.User {
	t.Helper()
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"nobody", "daemon", "www-data"} {
		u, err := user.Lookup(name)
		if err == nil && u.Uid != current.Uid {
			return u
		}
	}
	t.Skip("no secondary local user available")
	return nil
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
}
