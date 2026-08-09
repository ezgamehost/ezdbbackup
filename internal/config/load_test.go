package config

import (
	"strings"
	"testing"
)

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
