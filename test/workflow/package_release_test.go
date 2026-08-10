package workflow_test

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const (
	packageToolHelperEnv = "EZDBBACKUP_RELEASE_TOOL_HELPER"
	packageCallLogEnv    = "EZDBBACKUP_RELEASE_CALL_LOG"
	packageFailToolEnv   = "EZDBBACKUP_RELEASE_FAIL_TOOL"
	packageRealSHAEnv    = "EZDBBACKUP_RELEASE_REAL_SHA256SUM"
)

func TestMain(m *testing.M) {
	if os.Getenv(packageToolHelperEnv) == "1" {
		os.Exit(runPackageToolHelper())
	}
	os.Exit(m.Run())
}

func TestPackageReleaseBuildsAndInspectsBothArchitectures(t *testing.T) {
	fixture := newPackageFixture(t, "")
	output, err := fixture.run(t)
	if err != nil {
		t.Fatalf("package release: %v\n%s", err, output)
	}

	wantFiles := []string{
		"SHA256SUMS",
		"ezdbbackup_v9.8.7-test_linux_amd64.tar.gz",
		"ezdbbackup_v9.8.7-test_linux_arm64.tar.gz",
	}
	entries, err := os.ReadDir(fixture.dist)
	if err != nil {
		t.Fatal(err)
	}
	gotFiles := make([]string, 0, len(entries))
	for _, entry := range entries {
		gotFiles = append(gotFiles, entry.Name())
	}
	sort.Strings(gotFiles)
	if strings.Join(gotFiles, "\x00") != strings.Join(wantFiles, "\x00") {
		t.Fatalf("release files = %v, want %v", gotFiles, wantFiles)
	}

	for _, archive := range wantFiles[1:] {
		command := exec.Command("tar", "-tzf", filepath.Join(fixture.dist, archive))
		contents, err := command.Output()
		if err != nil {
			t.Fatalf("inspect %s: %v", archive, err)
		}
		if got, want := strings.Fields(string(contents)), []string{"ezdbbackup", "README.md", "config.example.yml"}; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("%s contents = %v, want %v", archive, got, want)
		}

		arch := "amd64"
		if strings.Contains(archive, "_arm64.") {
			arch = "arm64"
		}
		extract := exec.Command("tar", "-xOzf", filepath.Join(fixture.dist, archive), "ezdbbackup")
		binary, err := extract.Output()
		if err != nil {
			t.Fatalf("extract binary from %s: %v", archive, err)
		}
		if got, want := string(binary), fakeReleaseBinaryContents(arch); got != want {
			t.Errorf("%s binary was not the exact %s go build output", archive, arch)
		}
	}

	checksum := exec.Command(fixture.realSHA, "-c", "SHA256SUMS")
	checksum.Dir = fixture.dist
	if verified, err := checksum.CombinedOutput(); err != nil {
		t.Fatalf("verify packaged checksums: %v\n%s", err, verified)
	}
	checksumContents, err := os.ReadFile(filepath.Join(fixture.dist, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	checksumFiles := make([]string, 0, 2)
	for _, line := range strings.Split(strings.TrimSpace(string(checksumContents)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) != 64 {
			t.Fatalf("malformed SHA256SUMS line %q", line)
		}
		checksumFiles = append(checksumFiles, fields[1])
	}
	if want := wantFiles[1:]; strings.Join(checksumFiles, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("checksummed files = %v, want %v", checksumFiles, want)
	}

	records := readPackageCallLog(t, fixture.callLog)
	assertPackageCalls(t, records)
	assertPackageScratchEmpty(t, fixture.scratch)
}

func TestPackageReleaseFailsClosedWhenVerificationFails(t *testing.T) {
	tests := []struct {
		name string
		fail string
		want string
	}{
		{name: "build", fail: "build", want: "fake go build failure"},
		{name: "file format", fail: "file", want: "not a static amd64 Linux ELF executable"},
		{name: "architecture", fail: "arch", want: "not a static amd64 Linux ELF executable"},
		{name: "static linkage", fail: "linkage", want: "not a static amd64 Linux ELF executable"},
		{name: "dynamic dependency", fail: "ldd", want: "dynamic dependency reported"},
		{name: "version", fail: "version", want: "reports unexpected version"},
		{name: "checksum", fail: "checksum", want: "fake checksum verification failure"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPackageFixture(t, test.fail)
			output, err := fixture.run(t)
			assertPackageScratchEmpty(t, fixture.scratch)
			if err == nil {
				t.Fatalf("package release succeeded despite injected %s failure\n%s", test.fail, output)
			}
			if !strings.Contains(output, test.want) {
				t.Fatalf("package release output does not contain %q:\n%s", test.want, output)
			}
		})
	}
}

type packageFixture struct {
	fakeBin string
	dist    string
	scratch string
	callLog string
	realSHA string
	fail    string
}

func newPackageFixture(t *testing.T, fail string) packageFixture {
	t.Helper()
	root := t.TempDir()
	fakeBin := filepath.Join(root, "bin")
	dist := filepath.Join(root, "dist")
	scratch := filepath.Join(root, "tmp")
	for _, directory := range []string{fakeBin, dist, scratch} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"go", "file", "ldd", "sha256sum"} {
		if err := os.Symlink(testBinary, filepath.Join(fakeBin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	realSHA, err := exec.LookPath("sha256sum")
	if err != nil {
		t.Fatal(err)
	}

	return packageFixture{
		fakeBin: fakeBin,
		dist:    dist,
		scratch: scratch,
		callLog: filepath.Join(root, "calls.log"),
		realSHA: realSHA,
		fail:    fail,
	}
}

func (fixture packageFixture) run(t *testing.T) (string, error) {
	t.Helper()
	command := exec.Command("bash", filepath.Join(repositoryRoot(t), "scripts", "package-release.sh"))
	command.Dir = repositoryRoot(t)
	command.Env = packageEnvironment(map[string]string{
		"PATH":                        fixture.fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"TMPDIR":                      fixture.scratch,
		"GITHUB_REF_NAME":             "v9.8.7-test",
		"EZDBBACKUP_RELEASE_DIST_DIR": fixture.dist,
		packageToolHelperEnv:          "1",
		packageCallLogEnv:             fixture.callLog,
		packageFailToolEnv:            fixture.fail,
		packageRealSHAEnv:             fixture.realSHA,
	})
	output, err := command.CombinedOutput()
	return string(output), err
}

func packageEnvironment(overrides map[string]string) []string {
	environment := make([]string, 0, len(os.Environ())+len(overrides))
	for _, item := range os.Environ() {
		name, _, _ := strings.Cut(item, "=")
		if _, replaced := overrides[name]; !replaced {
			environment = append(environment, item)
		}
	}
	for name, value := range overrides {
		environment = append(environment, name+"="+value)
	}
	return environment
}

type packageCall struct {
	tool   string
	cgo    string
	goos   string
	goarch string
	args   []string
}

func readPackageCallLog(t *testing.T, path string) []packageCall {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	var records []packageCall
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.SplitN(scanner.Text(), "\t", 5)
		if len(fields) != 5 {
			t.Fatalf("malformed package tool record %q", scanner.Text())
		}
		arguments := []string(nil)
		if fields[4] != "" {
			arguments = strings.Split(fields[4], "\x1f")
		}
		records = append(records, packageCall{
			tool: fields[0], cgo: fields[1], goos: fields[2], goarch: fields[3], args: arguments,
		})
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return records
}

func assertPackageCalls(t *testing.T, records []packageCall) {
	t.Helper()
	counts := make(map[string]int)
	arches := make(map[string]bool)
	builds := make(map[string]string)
	for _, record := range records {
		counts[record.tool]++
		if record.tool == "go" {
			if record.cgo != "0" || record.goos != "linux" {
				t.Errorf("go build environment = CGO_ENABLED=%q GOOS=%q", record.cgo, record.goos)
			}
			arches[record.goarch] = true
			for _, required := range []string{
				"build", "-trimpath",
				"-ldflags=-s -w -X github.com/ezgamehost/ezdbbackup/internal/buildinfo.Version=v9.8.7-test",
				"./cmd/ezdbbackup",
			} {
				if !containsString(record.args, required) {
					t.Errorf("go build args %q do not contain exact argument %q", record.args, required)
				}
			}
			output := argumentAfter(record.args, "-o")
			if output == "" {
				t.Errorf("go build args %q have no -o target", record.args)
			} else if !strings.HasSuffix(output, filepath.Join("linux_"+record.goarch, "ezdbbackup")) {
				t.Errorf("%s go build output = %q", record.goarch, output)
			} else if prior := builds[record.goarch]; prior != "" {
				t.Errorf("duplicate %s build outputs %q and %q", record.goarch, prior, output)
			} else {
				builds[record.goarch] = output
			}
		}
	}
	for tool, want := range map[string]int{"go": 2, "file": 2, "ldd": 2, "version": 1, "sha256sum": 2} {
		if counts[tool] != want {
			t.Errorf("%s call count = %d, want %d (records: %#v)", tool, counts[tool], want, records)
		}
	}
	if !arches["amd64"] || !arches["arm64"] || len(arches) != 2 {
		t.Errorf("built architectures = %v, want amd64 and arm64", arches)
	}
	for _, tool := range []string{"file", "ldd"} {
		for arch, output := range builds {
			if !hasExactPackageCall(records, tool, []string{output}) {
				t.Errorf("%s did not inspect exact %s go build output %q", tool, arch, output)
			}
		}
	}
	if !hasExactPackageCall(records, "version", []string{builds["amd64"], "version"}) {
		t.Errorf("version probe did not execute exact amd64 go build output %q", builds["amd64"])
	}
	if hasExactPackageCall(records, "version", []string{builds["arm64"], "version"}) {
		t.Error("arm64 cross-compiled binary was unexpectedly executed")
	}
	wantArchives := []string{
		"ezdbbackup_v9.8.7-test_linux_amd64.tar.gz",
		"ezdbbackup_v9.8.7-test_linux_arm64.tar.gz",
	}
	if !hasExactPackageCall(records, "sha256sum", wantArchives) {
		t.Errorf("sha256 generation did not cover exact archives %v", wantArchives)
	}
	if !hasExactPackageCall(records, "sha256sum", []string{"-c", "SHA256SUMS"}) {
		t.Error("generated SHA256SUMS was not verified")
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func hasExactPackageCall(records []packageCall, tool string, arguments []string) bool {
	for _, record := range records {
		if record.tool == tool && strings.Join(record.args, "\x00") == strings.Join(arguments, "\x00") {
			return true
		}
	}
	return false
}

func assertPackageScratchEmpty(t *testing.T, scratch string) {
	t.Helper()
	remaining, err := os.ReadDir(scratch)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Errorf("temporary package directories were not cleaned up: %v", remaining)
	}
}

func runPackageToolHelper() int {
	tool := filepath.Base(os.Args[0])
	if err := appendPackageCall(tool, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 127
	}
	switch tool {
	case "go":
		if os.Getenv(packageFailToolEnv) == "build" {
			fmt.Fprintln(os.Stderr, "fake go build failure")
			return 1
		}
		output := argumentAfter(os.Args[1:], "-o")
		if output == "" {
			fmt.Fprintln(os.Stderr, "fake go did not receive -o")
			return 2
		}
		return writeFakeReleaseBinary(output, os.Getenv("GOARCH"))
	case "file":
		path := os.Args[len(os.Args)-1]
		contents, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		arch := ""
		switch {
		case strings.Contains(string(contents), "# fake-built-arch: amd64"):
			arch = "x86-64"
		case strings.Contains(string(contents), "# fake-built-arch: arm64"):
			arch = "ARM aarch64"
		default:
			fmt.Printf("%s: data\n", path)
			return 0
		}
		if os.Getenv(packageFailToolEnv) == "arch" && arch == "x86-64" {
			arch = "ARM aarch64"
		}
		linkage := "statically linked"
		if os.Getenv(packageFailToolEnv) == "linkage" {
			linkage = "dynamically linked"
		}
		format := "ELF 64-bit LSB executable"
		if os.Getenv(packageFailToolEnv) == "file" {
			format = "ASCII text"
		}
		fmt.Printf("%s: %s, %s, %s\n", path, format, arch, linkage)
		return 0
	case "ldd":
		if os.Getenv(packageFailToolEnv) == "ldd" {
			fmt.Println("libc.so.6 => /lib/libc.so.6")
			return 0
		}
		fmt.Println("not a dynamic executable")
		return 1
	case "sha256sum":
		if os.Getenv(packageFailToolEnv) == "checksum" && len(os.Args) > 1 && os.Args[1] == "-c" {
			fmt.Fprintln(os.Stderr, "fake checksum verification failure")
			return 1
		}
		command := exec.Command(os.Getenv(packageRealSHAEnv), os.Args[1:]...)
		command.Stdin = os.Stdin
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
		if err := command.Run(); err != nil {
			if exit, ok := err.(*exec.ExitError); ok {
				return exit.ExitCode()
			}
			fmt.Fprintln(os.Stderr, err)
			return 127
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown fake package tool %q\n", tool)
		return 127
	}
}

func appendPackageCall(tool string, args []string) error {
	file, err := os.OpenFile(os.Getenv(packageCallLogEnv), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = fmt.Fprintf(file, "%s\t%s\t%s\t%s\t%s\n",
		tool, os.Getenv("CGO_ENABLED"), os.Getenv("GOOS"), os.Getenv("GOARCH"), strings.Join(args, "\x1f"))
	return err
}

func argumentAfter(arguments []string, name string) string {
	for index := 0; index+1 < len(arguments); index++ {
		if arguments[index] == name {
			return arguments[index+1]
		}
	}
	return ""
}

func writeFakeReleaseBinary(path, arch string) int {
	contents := fakeReleaseBinaryContents(arch)
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func fakeReleaseBinaryContents(arch string) string {
	return `#!/usr/bin/env bash
set -eu
` + "# fake-built-arch: " + arch + `
printf 'version\t\t\t\t%s\037%s\n' "$0" "$1" >> "${EZDBBACKUP_RELEASE_CALL_LOG}"
if [[ "${EZDBBACKUP_RELEASE_FAIL_TOOL:-}" == "version" ]]; then
  printf 'ezdbbackup wrong-version\n'
else
  printf 'ezdbbackup %s\n' "${GITHUB_REF_NAME}"
fi
`
}
