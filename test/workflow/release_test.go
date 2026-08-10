package workflow_test

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type workflow struct {
	Name        string            `yaml:"name"`
	Permissions map[string]string `yaml:"permissions"`
	Jobs        map[string]job    `yaml:"jobs"`
	On          workflowTrigger   `yaml:"on"`
}

type workflowTrigger struct {
	Push struct {
		Tags []string `yaml:"tags"`
	} `yaml:"push"`
}

type job struct {
	Needs           stringList        `yaml:"needs"`
	RunsOn          string            `yaml:"runs-on"`
	Permissions     map[string]string `yaml:"permissions"`
	Environment     map[string]string `yaml:"env"`
	TimeoutMinutes  int               `yaml:"timeout-minutes"`
	If              string            `yaml:"if"`
	ContinueOnError bool              `yaml:"continue-on-error"`
	Steps           []step            `yaml:"steps"`
}

type step struct {
	Name            string         `yaml:"name"`
	Uses            string         `yaml:"uses"`
	With            map[string]any `yaml:"with"`
	Run             string         `yaml:"run"`
	If              string         `yaml:"if"`
	ContinueOnError bool           `yaml:"continue-on-error"`
	Environment     map[string]any `yaml:"env"`
}

type stringList []string

func (decoded *workflow) UnmarshalYAML(node *yaml.Node) error {
	if err := requireOnlyMappingKeys(node, "workflow", "name", "on", "permissions", "jobs"); err != nil {
		return err
	}
	type plainWorkflow workflow
	var value plainWorkflow
	if err := node.Decode(&value); err != nil {
		return err
	}
	*decoded = workflow(value)
	return nil
}

func (decoded *workflowTrigger) UnmarshalYAML(node *yaml.Node) error {
	if err := requireOnlyMappingKeys(node, "workflow trigger", "push"); err != nil {
		return err
	}
	push := mappingValue(node, "push")
	if err := requireOnlyMappingKeys(push, "push trigger", "tags"); err != nil {
		return err
	}
	if err := mappingValue(push, "tags").Decode(&decoded.Push.Tags); err != nil {
		return fmt.Errorf("decode release tag filter: %w", err)
	}
	return nil
}

func (decoded *job) UnmarshalYAML(node *yaml.Node) error {
	if err := requireOnlyMappingKeys(node, "release job", "needs", "runs-on", "timeout-minutes", "permissions", "env", "steps"); err != nil {
		return err
	}
	type plainJob job
	var value plainJob
	if err := node.Decode(&value); err != nil {
		return err
	}
	*decoded = job(value)
	return nil
}

func (decoded *step) UnmarshalYAML(node *yaml.Node) error {
	if err := requireOnlyMappingKeys(node, "release step", "name", "uses", "with", "run", "if", "continue-on-error", "env"); err != nil {
		return err
	}
	type plainStep step
	var value plainStep
	if err := node.Decode(&value); err != nil {
		return err
	}
	*decoded = step(value)
	return nil
}

type composeFile struct {
	Services map[string]composeService `yaml:"services"`
}

type composeService struct {
	Image       string             `yaml:"image"`
	Environment map[string]string  `yaml:"environment"`
	Healthcheck composeHealthcheck `yaml:"healthcheck"`
}

type composeHealthcheck struct {
	Test        []string `yaml:"test"`
	Interval    string   `yaml:"interval"`
	Timeout     string   `yaml:"timeout"`
	Retries     int      `yaml:"retries"`
	StartPeriod string   `yaml:"start_period"`
}

func (values *stringList) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case 0:
		return nil
	case yaml.ScalarNode:
		*values = []string{node.Value}
		return nil
	case yaml.SequenceNode:
		decoded := make([]string, len(node.Content))
		for index, item := range node.Content {
			if item.Kind != yaml.ScalarNode || item.Tag != "!!str" {
				return fmt.Errorf("needs[%d] must be a string", index)
			}
			decoded[index] = item.Value
		}
		*values = decoded
		return nil
	default:
		return errorsForNode(node, "needs must be a string or sequence")
	}
}

func requireOnlyMappingKeys(node *yaml.Node, label string, allowed ...string) error {
	if node == nil || node.Kind != yaml.MappingNode || node.Tag != "!!map" {
		return errorsForNode(node, label+" must be a mapping")
	}
	allowedKeys := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		allowedKeys[key] = true
	}
	for index := 0; index < len(node.Content); index += 2 {
		key := node.Content[index]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || !allowedKeys[key.Value] {
			return errorsForNode(key, fmt.Sprintf("%s contains unapproved key %q", label, key.Value))
		}
	}
	return nil
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node != nil {
		for index := 0; index+1 < len(node.Content); index += 2 {
			if node.Content[index].Value == key {
				return node.Content[index+1]
			}
		}
	}
	return &yaml.Node{}
}

func TestReleasePublishDependsOnEveryVerificationGate(t *testing.T) {
	release := loadReleaseWorkflow(t)
	if release.Name != "Release" {
		t.Fatalf("release workflow name = %q, want Release", release.Name)
	}
	if !reflect.DeepEqual(release.On.Push.Tags, []string{"v*"}) {
		t.Fatalf("release tag trigger = %v, want only v*", release.On.Push.Tags)
	}
	if len(release.Permissions) != 0 {
		t.Fatalf("top-level permissions = %v, want an empty default", release.Permissions)
	}

	gates := []string{"unit", "vet", "race", "static-build", "integration"}
	allJobs := append(append([]string(nil), gates...), "publish")
	jobNames := make([]string, 0, len(release.Jobs))
	for name := range release.Jobs {
		jobNames = append(jobNames, name)
	}
	assertExactStrings(t, "release jobs", jobNames, allJobs)
	for _, name := range gates {
		verification, ok := release.Jobs[name]
		if !ok {
			t.Errorf("release workflow has no %q verification job", name)
			continue
		}
		assertExactPermission(t, name, verification.Permissions, "contents", "read")
		assertJobCannotIgnoreFailures(t, name, verification)
		if len(verification.Needs) != 0 {
			t.Errorf("%s job unexpectedly depends on %v", name, verification.Needs)
		}
	}

	publish, ok := release.Jobs["publish"]
	if !ok {
		t.Fatal("release workflow has no publish job")
	}
	assertExactPermission(t, "publish", publish.Permissions, "contents", "write")
	assertJobCannotIgnoreFailures(t, "publish", publish)
	assertExactStrings(t, "publish.needs", []string(publish.Needs), gates)
	if len(publish.Steps) != 3 {
		t.Fatalf("publish steps = %d, want download, checksum, and publish only", len(publish.Steps))
	}
}

func TestReleaseVerificationJobsExerciseRequiredBoundaries(t *testing.T) {
	release := loadReleaseWorkflow(t)
	for name, timeout := range map[string]int{
		"unit": 15, "vet": 15, "race": 20, "static-build": 15, "integration": 20, "publish": 10,
	} {
		candidate := release.Jobs[name]
		if candidate.RunsOn != "ubuntu-latest" || candidate.TimeoutMinutes != timeout {
			t.Errorf("%s runner/timeout = %q/%d, want ubuntu-latest/%d", name, candidate.RunsOn, candidate.TimeoutMinutes, timeout)
		}
		if name != "integration" && len(candidate.Environment) != 0 {
			t.Errorf("%s job has unapproved environment: %v", name, candidate.Environment)
		}
	}

	assertExactRun(t, release.Jobs["unit"], "Unit tests", "go test -count=1 ./...")
	assertExactRun(t, release.Jobs["vet"], "Vet", "go vet ./...")
	assertExactRun(t, release.Jobs["race"], "Race tests", "go test -count=1 -race ./...")

	integration := release.Jobs["integration"]
	if integration.TimeoutMinutes <= 0 {
		t.Fatal("integration job has no timeout-minutes bound")
	}
	wantEnvironment := map[string]string{
		"AWS_ACCESS_KEY_ID":           "test",
		"AWS_SECRET_ACCESS_KEY":       "test",
		"AWS_REGION":                  "us-east-1",
		"EZDBBACKUP_TEST_S3_ENDPOINT": "http://127.0.0.1:4566",
		"COMPOSE_PROJECT_NAME":        "ezdbbackup-release-${{ github.run_id }}-${{ github.run_attempt }}",
	}
	if !reflect.DeepEqual(integration.Environment, wantEnvironment) {
		t.Fatalf("integration env = %v, want %v", integration.Environment, wantEnvironment)
	}
	assertExactRun(t, integration, "Start LocalStack", "docker compose -f test/compose.yml up -d --wait --wait-timeout 120")
	assertExactRun(t, integration, "Tagged LocalStack integration suite", "go test -count=1 -tags=integration ./test/e2e -v")
	stop := findStep(t, integration, "Stop LocalStack")
	if stop.If != "always()" {
		t.Fatalf("Stop LocalStack if = %q, want always()", stop.If)
	}
	if strings.TrimSpace(stop.Run) != "docker compose -f test/compose.yml down -v --remove-orphans" {
		t.Fatalf("Stop LocalStack run = %q", stop.Run)
	}

	build := release.Jobs["static-build"]
	assertExactRun(t, build, "Build, inspect, and package static Linux binaries", "bash scripts/package-release.sh")
	findUses(t, build, "actions/upload-artifact@v4")

	publish := release.Jobs["publish"]
	findUses(t, publish, "actions/download-artifact@v4")
	assertExactRun(t, publish, "Verify packaged checksums", "cd dist && sha256sum -c SHA256SUMS")
	publishStep := findStep(t, publish, "Publish GitHub release")
	if strings.TrimSpace(publishStep.Run) != `gh release create "$GITHUB_REF_NAME" dist/* --verify-tag --generate-notes` {
		t.Fatalf("publish command = %q", publishStep.Run)
	}

	compose := loadComposeFile(t)
	localstack, ok := compose.Services["localstack"]
	if !ok {
		t.Fatal("test Compose file has no localstack service")
	}
	if localstack.Image != "localstack/localstack:4.14.0" {
		t.Fatalf("LocalStack image = %q, want pinned localstack/localstack:4.14.0", localstack.Image)
	}
	if localstack.Environment["SERVICES"] != "s3" {
		t.Fatalf("LocalStack services = %q, want only s3", localstack.Environment["SERVICES"])
	}
	health := localstack.Healthcheck
	wantHealth := composeHealthcheck{
		Test:        []string{"CMD", "curl", "-fsS", "http://localhost:4566/_localstack/health"},
		Interval:    "2s",
		Timeout:     "2s",
		Retries:     30,
		StartPeriod: "5s",
	}
	if !reflect.DeepEqual(health, wantHealth) {
		t.Fatalf("LocalStack healthcheck = %#v, want %#v", health, wantHealth)
	}

	setupGo := step{Uses: "actions/setup-go@v5", With: map[string]any{"go-version-file": "go.mod"}}
	checkout := step{Uses: "actions/checkout@v4"}
	assertExactSteps(t, "unit", release.Jobs["unit"], []step{
		checkout,
		setupGo,
		{Name: "Unit tests", Run: "go test -count=1 ./..."},
	})
	assertExactSteps(t, "vet", release.Jobs["vet"], []step{
		checkout,
		setupGo,
		{Name: "Vet", Run: "go vet ./..."},
	})
	assertExactSteps(t, "race", release.Jobs["race"], []step{
		checkout,
		setupGo,
		{Name: "Race tests", Run: "go test -count=1 -race ./..."},
	})
	assertExactSteps(t, "static-build", release.Jobs["static-build"], []step{
		checkout,
		setupGo,
		{Name: "Build, inspect, and package static Linux binaries", Run: "bash scripts/package-release.sh"},
		{
			Name: "Upload verified release artifacts", Uses: "actions/upload-artifact@v4",
			With: map[string]any{
				"name": "release-dist", "path": "dist/", "if-no-files-found": "error",
				"retention-days": 1, "compression-level": 0,
			},
		},
	})
	assertExactSteps(t, "integration", integration, []step{
		checkout,
		setupGo,
		{Name: "Docker versions", Run: "docker version\ndocker compose version"},
		{Name: "Start LocalStack", Run: "docker compose -f test/compose.yml up -d --wait --wait-timeout 120"},
		{Name: "Tagged LocalStack integration suite", Run: "go test -count=1 -tags=integration ./test/e2e -v"},
		{Name: "Stop LocalStack", If: "always()", Run: "docker compose -f test/compose.yml down -v --remove-orphans"},
	})
	assertExactSteps(t, "publish", publish, []step{
		{
			Name: "Download verified release artifacts", Uses: "actions/download-artifact@v4",
			With: map[string]any{"name": "release-dist", "path": "dist"},
		},
		{Name: "Verify packaged checksums", Run: "cd dist && sha256sum -c SHA256SUMS"},
		{
			Name: "Publish GitHub release", Run: `gh release create "$GITHUB_REF_NAME" dist/* --verify-tag --generate-notes`,
			Environment: map[string]any{
				"GH_TOKEN": "${{ github.token }}", "GH_REPO": "${{ github.repository }}",
			},
		},
	})
}

func loadReleaseWorkflow(t *testing.T) workflow {
	t.Helper()
	path := filepath.Join(repositoryRoot(t), ".github", "workflows", "release.yml")
	var decoded workflow
	loadStrictYAMLDocument(t, path, &decoded)
	return decoded
}

func loadComposeFile(t *testing.T) composeFile {
	t.Helper()
	path := filepath.Join(repositoryRoot(t), "test", "compose.yml")
	var decoded composeFile
	loadStrictYAMLDocument(t, path, &decoded)
	return decoded
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate release workflow test")
	}
	return filepath.Join(filepath.Dir(current), "..", "..")
}

func loadStrictYAMLDocument(t *testing.T, path string, destination any) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	decoder := yaml.NewDecoder(bytes.NewReader(contents))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if err := rejectIndirectionAndDuplicateKeys(&document); err != nil {
		t.Fatalf("unsafe YAML in %s: %v", path, err)
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("%s must contain exactly one YAML document: %v", path, err)
	}

	if err := document.Decode(destination); err != nil {
		t.Fatalf("decode %s structure: %v", path, err)
	}
}

func rejectIndirectionAndDuplicateKeys(node *yaml.Node) error {
	if node.Kind == yaml.AliasNode || node.Alias != nil {
		return errorsForNode(node, "aliases are not allowed")
	}
	if node.Tag != "" && !strings.HasPrefix(node.Tag, "!!") {
		return errorsForNode(node, "custom YAML tags are not allowed")
	}
	if node.Kind == yaml.MappingNode {
		seen := make(map[string]struct{}, len(node.Content)/2)
		for index := 0; index < len(node.Content); index += 2 {
			key := node.Content[index]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return errorsForNode(key, "mapping keys must be strings")
			}
			if _, exists := seen[key.Value]; exists {
				return errorsForNode(key, "duplicate mapping key")
			}
			seen[key.Value] = struct{}{}
		}
	}
	for _, child := range node.Content {
		if err := rejectIndirectionAndDuplicateKeys(child); err != nil {
			return err
		}
	}
	return nil
}

func errorsForNode(node *yaml.Node, message string) error {
	return fmt.Errorf("line %d, column %d: %s", node.Line, node.Column, message)
}

func assertExactPermission(t *testing.T, jobName string, permissions map[string]string, key, value string) {
	t.Helper()
	if len(permissions) != 1 || permissions[key] != value {
		t.Errorf("%s permissions = %v, want only %s: %s", jobName, permissions, key, value)
	}
}

func assertJobCannotIgnoreFailures(t *testing.T, name string, candidate job) {
	t.Helper()
	if candidate.If != "" {
		t.Errorf("%s job has conditional execution %q", name, candidate.If)
	}
	if candidate.ContinueOnError {
		t.Errorf("%s job has continue-on-error enabled", name)
	}
	for _, item := range candidate.Steps {
		if item.ContinueOnError {
			t.Errorf("%s step %q has continue-on-error enabled", name, item.Name)
		}
		if item.If != "" && !(name == "integration" && item.Name == "Stop LocalStack" && item.If == "always()") {
			t.Errorf("%s step %q has conditional execution %q", name, item.Name, item.If)
		}
	}
}

func assertExactStrings(t *testing.T, label string, got, want []string) {
	t.Helper()
	got = append([]string(nil), got...)
	want = append([]string(nil), want...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
}

func assertExactRun(t *testing.T, candidate job, name, want string) {
	t.Helper()
	item := findStep(t, candidate, name)
	if strings.TrimSpace(item.Run) != want {
		t.Fatalf("%s run = %q, want %q", name, item.Run, want)
	}
}

func assertExactSteps(t *testing.T, jobName string, candidate job, want []step) {
	t.Helper()
	got := append([]step(nil), candidate.Steps...)
	for index := range got {
		got[index].Run = strings.TrimSpace(got[index].Run)
	}
	for index := range want {
		want[index].Run = strings.TrimSpace(want[index].Run)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s steps =\n%#v\nwant\n%#v", jobName, got, want)
	}
}

func findStep(t *testing.T, candidate job, name string) step {
	t.Helper()
	for _, item := range candidate.Steps {
		if item.Name == name {
			return item
		}
	}
	t.Fatalf("job has no %q step", name)
	return step{}
}

func findUses(t *testing.T, candidate job, action string) step {
	t.Helper()
	for _, item := range candidate.Steps {
		if item.Uses == action {
			return item
		}
	}
	t.Fatalf("job does not use %q", action)
	return step{}
}
