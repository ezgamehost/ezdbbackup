package validation

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
	dir := t.TempDir()
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

func TestOSEnvironmentExecutableUsesIntendedUserPermissions(t *testing.T) {
	requireLinux(t)
	other := lookupDifferentUser(t)
	path := filepath.Join(t.TempDir(), "owner-only")
	writeExecutable(t, path, "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}

	err := (OSEnvironment{}).CheckExecutableAs(context.Background(), path, other.Username)
	if err == nil || !strings.Contains(err.Error(), other.Username) {
		t.Fatalf("CheckExecutable(owner-only as %q) error = %v, want intended-user permission error", other.Username, err)
	}
}

func TestOSEnvironmentRejectsSymlinkTargets(t *testing.T) {
	requireLinux(t)
	runAs := currentUsername(t)
	env := OSEnvironment{}
	dir := t.TempDir()

	executableTarget := filepath.Join(dir, "real-mysqldump")
	writeExecutable(t, executableTarget, "#!/bin/sh\nexit 0\n")
	executableLink := filepath.Join(dir, "mysqldump")
	if err := os.Symlink(executableTarget, executableLink); err != nil {
		t.Fatal(err)
	}
	if err := env.CheckExecutableAs(context.Background(), executableLink, runAs); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("CheckExecutableAs(symlink) error = %v, want symbolic-link error", err)
	}

	secretTarget := filepath.Join(dir, "real-secret")
	if err := os.WriteFile(secretTarget, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	secretLink := filepath.Join(dir, "secret")
	if err := os.Symlink(secretTarget, secretLink); err != nil {
		t.Fatal(err)
	}
	if err := env.CheckSecretFile(secretLink, runAs); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("CheckSecretFile(symlink) error = %v, want symbolic-link error", err)
	}

	directoryTarget := t.TempDir()
	directoryLink := filepath.Join(dir, "writable")
	if err := os.Symlink(directoryTarget, directoryLink); err != nil {
		t.Fatal(err)
	}
	if err := env.CheckWritableTarget(directoryLink, runAs); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("CheckWritableTarget(symlink) error = %v, want symbolic-link error", err)
	}
}

func TestOSEnvironmentRejectsSymlinkedParentComponents(t *testing.T) {
	requireLinux(t)
	runAs := currentUsername(t)
	env := OSEnvironment{}
	root := t.TempDir()
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
	if err := env.CheckExecutableAs(context.Background(), filepath.Join(linkedParent, "mysqldump"), runAs); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("CheckExecutableAs(symlinked parent) error = %v, want symbolic-link error", err)
	}

	secret := filepath.Join(realParent, "secret")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := env.CheckSecretFile(filepath.Join(linkedParent, "secret"), runAs); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("CheckSecretFile(symlinked parent) error = %v, want symbolic-link error", err)
	}

	existingDirectory := filepath.Join(realParent, "existing")
	if err := os.Mkdir(existingDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := env.CheckWritableTarget(filepath.Join(linkedParent, "existing"), runAs); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("CheckWritableTarget(existing under symlinked parent) error = %v, want symbolic-link error", err)
	}

	if err := env.CheckWritableTarget(filepath.Join(linkedParent, "missing", "target"), runAs); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("CheckWritableTarget(missing under symlinked parent) error = %v, want symbolic-link error", err)
	}
}

func TestOSEnvironmentRejectsSymlinkBeforeDotDotComponent(t *testing.T) {
	requireLinux(t)
	runAs := currentUsername(t)
	root := t.TempDir()
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
	locked := t.TempDir()
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

func TestOSEnvironmentWritableTargetUsesExistingDirectoryOrNearestParent(t *testing.T) {
	requireLinux(t)
	runAs := currentUsername(t)
	env := OSEnvironment{}
	writable := t.TempDir()
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
	locked := t.TempDir()
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

func TestOSEnvironmentSecretFileRules(t *testing.T) {
	requireLinux(t)
	runAs := currentUsername(t)
	env := OSEnvironment{}
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("secret"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := env.CheckSecretFile(path, runAs); err != nil {
		t.Fatalf("CheckSecretFile(valid) error = %v", err)
	}
	if err := env.CheckSecretFile("relative", runAs); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("CheckSecretFile(relative) error = %v, want absolute error", err)
	}
	if err := env.CheckSecretFile(t.TempDir(), runAs); err == nil || !strings.Contains(err.Error(), "regular") {
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

func TestOSEnvironmentUserAndCronPathRules(t *testing.T) {
	env := OSEnvironment{}
	if err := env.CheckUser(currentUsername(t)); err != nil {
		t.Fatalf("CheckUser(current) error = %v", err)
	}
	if err := env.CheckUser("ezdbbackup-user-that-does-not-exist"); err == nil {
		t.Fatal("CheckUser(missing) error = nil")
	}

	for _, valid := range []string{"/usr/local/bin/ezdbbackup", "/etc/ezdbbackup/config.yml"} {
		if err := env.CheckCronPath(valid); err != nil {
			t.Fatalf("CheckCronPath(%q) error = %v", valid, err)
		}
	}
	for _, invalid := range []string{"relative", "/tmp/bad\npath", "/tmp/bad\x00path"} {
		if err := env.CheckCronPath(invalid); err == nil {
			t.Fatalf("CheckCronPath(%q) error = nil", invalid)
		}
	}
}

func requireLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("ezdbbackup v1 supports Linux only")
	}
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
