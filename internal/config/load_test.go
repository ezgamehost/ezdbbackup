package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Removing the bounded reader from Load or Decode would let a local config
// source consume memory without limit before YAML validation begins.
func TestConfigurationReadsAreBounded(t *testing.T) {
	oversized := strings.Repeat("#", int(MaxFileBytes)+1)
	if _, findings := Decode(strings.NewReader(oversized)); !findings.HasErrors() || !strings.Contains(findings.Error(), "too large") {
		t.Fatalf("Decode(oversized) findings = %v, want size rejection", findings)
	}

	path := filepath.Join(secureConfigTestDir(t), "config.yml")
	if err := os.WriteFile(path, []byte(oversized), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, findings := Load(path); !findings.HasErrors() || !strings.Contains(findings.Error(), "too large") {
		t.Fatalf("Load(oversized) findings = %v, want size rejection", findings)
	}
}

// Reopening the configured pathname after decoding would lose this canonical
// identity and reintroduce a validation-to-use substitution window.
func TestLoadRecordsAndRechecksPinnedCanonicalSource(t *testing.T) {
	directory := secureConfigTestDir(t)
	target := filepath.Join(directory, "config-target.yml")
	if err := os.WriteFile(target, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "config.yml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	cfg, findings := Load(link)
	if findings.HasErrors() {
		t.Fatalf("Load() findings = %v", findings)
	}
	source, ok := cfg.Source()
	if !ok {
		t.Fatal("Load() config has no pinned source metadata")
	}
	if source.CanonicalPath != target {
		t.Fatalf("canonical source = %q, want %q", source.CanonicalPath, target)
	}
	if got := cfg.TrustedPath(link); got != target {
		t.Fatalf("TrustedPath() = %q, want %q", got, target)
	}

	original := target + ".original"
	if err := os.Rename(target, original); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("version: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cfg.RecheckSource(); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("RecheckSource() error = %v, want identity-change rejection", err)
	}
}

// A source must not be mutable through its own metadata or a replaceable
// canonical ancestor. A sticky shared boundary remains safe when the entry is
// owned by the trusted loading identity.
func TestLoadRejectsUnsafeSourceMetadataAndCanonicalAncestors(t *testing.T) {
	t.Run("group writable file", func(t *testing.T) {
		path := filepath.Join(secureConfigTestDir(t), "config.yml")
		if err := os.WriteFile(path, []byte("version: 1\n"), 0o660); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o660); err != nil {
			t.Fatal(err)
		}
		if _, findings := Load(path); !findings.HasErrors() || !strings.Contains(findings.Error(), "writable") {
			t.Fatalf("Load(group-writable) findings = %v, want rejection", findings)
		}
	})

	t.Run("non-sticky writable ancestor", func(t *testing.T) {
		parent := filepath.Join(secureConfigTestDir(t), "shared")
		if err := os.Mkdir(parent, 0o770); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(parent, 0o770); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(parent, "config.yml")
		if err := os.WriteFile(path, []byte("version: 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, findings := Load(path); !findings.HasErrors() || !strings.Contains(findings.Error(), "ancestor") {
			t.Fatalf("Load(replaceable ancestor) findings = %v, want rejection", findings)
		}
	})

	t.Run("sticky boundary and safe symlink", func(t *testing.T) {
		root := secureConfigTestDir(t)
		shared := filepath.Join(root, "shared")
		if err := os.Mkdir(shared, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(shared, os.ModeSticky|0o777); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(shared, "trusted.yml")
		if err := os.WriteFile(target, []byte("version: 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(root, "distro-config.yml")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, findings := Load(link); findings.HasErrors() {
			t.Fatalf("Load(sticky safe symlink) findings = %v", findings)
		}
	})
}

func secureConfigTestDir(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return directory
}

func TestDecodeValidConfigAppliesDefaults(t *testing.T) {
	input := `
version: 1
jobs:
  production:
    enabled: true
    schedule: "0 2 * * *"
    run_as: root
    mysql:
      host: db.internal
      user: backup
      databases: all
    s3:
      bucket: backups
      region: us-east-1
  archive:
    enabled: false
    schedule: "0 3 * * *"
    run_as: backup
    mysql:
      host: archive.internal
      user: backup
      databases: [events]
    s3:
      bucket: backups
      prefix: /archive//mysql/
      region: us-east-1
`

	cfg, findings := Decode(strings.NewReader(input))
	if findings.HasErrors() {
		t.Fatalf("Decode() findings = %v", findings)
	}
	job := cfg.Jobs["production"]
	if job.MySQL.Port != 3306 ||
		job.DumpBinary != "/usr/bin/mysqldump" ||
		job.TempDir != "/var/lib/ezdbbackup/tmp" ||
		!job.MySQL.Databases.All {
		t.Fatalf("defaults not applied: %#v", job)
	}
	if got, want := cfg.Logging.Directory, "/var/log/ezdbbackup"; got != want {
		t.Fatalf("logging directory = %q, want %q", got, want)
	}
	if got, want := cfg.Logging.Rotation, (RotationConfig{MaxSizeMB: 100, MaxFiles: 7, MaxAgeDays: 30, Compress: true}); got != want {
		t.Fatalf("rotation = %#v, want %#v", got, want)
	}
	if got, want := cfg.Jobs["archive"].MySQL.Databases.Names, []string{"events"}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("database names = %#v, want %#v", got, want)
	}
	if got, want := cfg.Jobs["archive"].S3.Prefix, "archive/mysql"; got != want {
		t.Fatalf("prefix = %q, want %q", got, want)
	}
	if got, want := cfg.JobNames(), []string{"archive", "production"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("JobNames() = %#v, want %#v", got, want)
	}
	if got, want := cfg.EnabledJobNames(), []string{"production"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("EnabledJobNames() = %#v, want %#v", got, want)
	}
}

func TestDecodePreservesExplicitZeroValuesForValidation(t *testing.T) {
	input := `
version: 1
logging:
  rotation:
    max_size_mb: 0
    max_files: 0
    max_age_days: 0
jobs:
  production:
    enabled: true
    schedule: "0 2 * * *"
    run_as: root
    mysql:
      host: db.internal
      port: 0
      user: backup
      databases: all
    s3:
      bucket: backups
      region: us-east-1
`

	cfg, findings := Decode(strings.NewReader(input))
	if findings.HasErrors() {
		t.Fatalf("Decode() findings = %v", findings)
	}
	if got := cfg.Logging.Rotation.MaxSizeMB; got != 0 {
		t.Fatalf("MaxSizeMB = %d, want explicit zero", got)
	}
	if got := cfg.Logging.Rotation.MaxFiles; got != 0 {
		t.Fatalf("MaxFiles = %d, want explicit zero", got)
	}
	if got := cfg.Logging.Rotation.MaxAgeDays; got != 0 {
		t.Fatalf("MaxAgeDays = %d, want explicit zero", got)
	}
	if got := cfg.Jobs["production"].MySQL.Port; got != 0 {
		t.Fatalf("port = %d, want explicit zero", got)
	}
	if findings := Validate(cfg); !findings.HasErrors() {
		t.Fatalf("Validate() findings = %v, want errors for explicit zero values", findings)
	}
}

func TestDecodeRejectsInvalidYAMLDocuments(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{name: "unknown field", input: "version: 1\nunknown: true\n"},
		{name: "duplicate key", input: "version: 1\nversion: 1\n"},
		{name: "second document", input: "version: 1\n---\nversion: 1\n"},
		{name: "malformed YAML", input: "version: [\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, findings := Decode(strings.NewReader(tt.input))
			if !findings.HasErrors() {
				t.Fatalf("Decode() findings = %v, want error", findings)
			}
		})
	}
}

func TestDatabaseSelectionRejectsInvalidValues(t *testing.T) {
	tests := []string{
		"databases: ALL\n",
		"databases: []\n",
		"databases: [app, '']\n",
		"databases: app\n",
	}
	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			_, findings := Decode(strings.NewReader("version: 1\njobs:\n  production:\n    mysql:\n      " + input))
			if !findings.HasErrors() {
				t.Fatalf("Decode(%q) findings = %v, want error", input, findings)
			}
		})
	}
}

// This fails if YAML nulls, scalar coercions, aliases/merges, or wrong
// collection kinds silently become Go zero values before validation.
func TestDecodeRejectsNonCanonicalYAMLTypesAndIndirection(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{name: "numeric string version", input: "version: '1'\n"},
		{name: "numeric string port", input: "version: 1\njobs:\n  production:\n    mysql:\n      port: '3307'\n"},
		{name: "numeric string rotation", input: "version: 1\nlogging:\n  rotation:\n    max_size_mb: '10'\n"},
		{name: "numeric host coerced to string", input: "version: 1\njobs:\n  production:\n    mysql:\n      host: 127\n"},
		{name: "boolean bucket coerced to string", input: "version: 1\njobs:\n  production:\n    s3:\n      bucket: true\n"},
		{name: "null debug boolean", input: "version: 1\nlogging:\n  debug: null\n"},
		{name: "null rotation boolean", input: "version: 1\nlogging:\n  rotation:\n    compress: null\n"},
		{name: "null enabled boolean", input: "version: 1\njobs:\n  production:\n    enabled: null\n"},
		{name: "null path style boolean", input: "version: 1\njobs:\n  production:\n    s3:\n      force_path_style: null\n"},
		{name: "null string", input: "version: 1\njobs:\n  production:\n    mysql:\n      host: null\n"},
		{name: "null integer", input: "version: 1\njobs:\n  production:\n    mysql:\n      port: null\n"},
		{name: "null string sequence", input: "version: 1\njobs:\n  production:\n    mysql:\n      extra_args: null\n"},
		{name: "jobs sequence", input: "version: 1\njobs: []\n"},
		{name: "extra args mapping", input: "version: 1\njobs:\n  production:\n    mysql:\n      extra_args: {}\n"},
		{name: "extra args numeric item", input: "version: 1\njobs:\n  production:\n    mysql:\n      extra_args: [1]\n"},
		{name: "databases mapping", input: "version: 1\njobs:\n  production:\n    mysql:\n      databases: {}\n"},
		{name: "custom tagged root mapping", input: "!unsafe {version: 1}\n"},
		{name: "custom tagged jobs mapping", input: "version: 1\njobs: !unsafe {}\n"},
		{name: "custom tagged extra args sequence", input: "version: 1\njobs:\n  production:\n    mysql:\n      extra_args: !unsafe [--quick]\n"},
		{name: "custom tagged database sequence", input: "version: 1\njobs:\n  production:\n    mysql:\n      databases: !unsafe [application]\n"},
		{
			name: "aliased port",
			input: "version: &port 1\n" +
				"jobs:\n  production:\n    mysql:\n      port: *port\n",
		},
		{
			name: "merged port",
			input: "version: 1\n" +
				"jobs:\n  production:\n    mysql:\n      <<: {port: 3307}\n",
		},
		{
			name: "merged rotation",
			input: "version: 1\n" +
				"logging:\n  rotation:\n    <<: {max_size_mb: 1, max_files: 2, max_age_days: 3, compress: false}\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, findings := Decode(strings.NewReader(tt.input))
			if !findings.HasErrors() {
				t.Fatalf("Decode() findings = %v, want strict YAML type error", findings)
			}
		})
	}
}
