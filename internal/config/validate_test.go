package config

import (
	"math"
	"testing"
	"time"
)

func validConfig() *Config {
	return &Config{
		Version:  1,
		Defaults: Defaults{DumpBinary: "/usr/bin/mysqldump", TempDir: "/var/lib/ezdbbackup/tmp"},
		Logging:  LoggingConfig{Directory: "/var/log/ezdbbackup", Rotation: RotationConfig{MaxSizeMB: 100, MaxFiles: 7, MaxAgeDays: 30, Compress: true}},
		Jobs: map[string]JobConfig{
			"production": {
				Enabled: true, Schedule: "0 2 * * *", RunAs: "root", DumpBinary: "/usr/bin/mysqldump", TempDir: "/var/lib/ezdbbackup/tmp",
				MySQL: MySQLConfig{Host: "db.internal", Port: 3306, User: "backup", Databases: DatabaseSelection{All: true}},
				S3:    S3Config{Bucket: "backups", Region: "us-east-1"},
			},
		},
	}
}

func TestValidateRejectsInvalidConfiguration(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Config)
	}{
		{"wrong version", func(c *Config) { c.Version = 2 }},
		{"invalid job name", func(c *Config) { c.Jobs["invalid/name"] = c.Jobs["production"]; delete(c.Jobs, "production") }},
		{"invalid five-field cron", func(c *Config) { j := c.Jobs["production"]; j.Schedule = "0 2 * *"; c.Jobs["production"] = j }},
		{"cron nickname", func(c *Config) { j := c.Jobs["production"]; j.Schedule = "@daily"; c.Jobs["production"] = j }},
		{"missing run user", func(c *Config) { j := c.Jobs["production"]; j.RunAs = ""; c.Jobs["production"] = j }},
		{"relative paths", func(c *Config) { j := c.Jobs["production"]; j.TempDir = "tmp"; c.Jobs["production"] = j }},
		{"missing database scope", func(c *Config) {
			j := c.Jobs["production"]
			j.MySQL.Databases = DatabaseSelection{}
			c.Jobs["production"] = j
		}},
		{"ambiguous database scope", func(c *Config) {
			j := c.Jobs["production"]
			j.MySQL.Databases = DatabaseSelection{All: true, Names: []string{"app"}}
			c.Jobs["production"] = j
		}},
		{"both password forms", func(c *Config) {
			j := c.Jobs["production"]
			j.MySQL.Password = "secret"
			j.MySQL.PasswordFile = "/etc/password"
			c.Jobs["production"] = j
		}},
		{"owned dump argument", func(c *Config) {
			j := c.Jobs["production"]
			j.MySQL.ExtraArgs = []string{"--host=other"}
			c.Jobs["production"] = j
		}},
		{"output dump argument", func(c *Config) {
			j := c.Jobs["production"]
			j.MySQL.ExtraArgs = []string{"--output=/tmp/dump"}
			c.Jobs["production"] = j
		}},
		{"positional database", func(c *Config) {
			j := c.Jobs["production"]
			j.MySQL.ExtraArgs = []string{"otherdb"}
			c.Jobs["production"] = j
		}},
		{"missing S3 bucket", func(c *Config) { j := c.Jobs["production"]; j.S3.Bucket = ""; c.Jobs["production"] = j }},
		{"missing S3 region", func(c *Config) { j := c.Jobs["production"]; j.S3.Region = ""; c.Jobs["production"] = j }},
		{"malformed endpoint", func(c *Config) { j := c.Jobs["production"]; j.S3.Endpoint = "s3.example.com"; c.Jobs["production"] = j }},
		{"incomplete explicit S3 credentials", func(c *Config) { j := c.Jobs["production"]; j.S3.AccessKeyID = "key"; c.Jobs["production"] = j }},
		{"literal file credential conflict", func(c *Config) {
			j := c.Jobs["production"]
			j.S3.SecretAccessKey = "secret"
			j.S3.SecretAccessKeyFile = "/etc/secret"
			c.Jobs["production"] = j
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.edit(cfg)
			if findings := Validate(cfg); !findings.HasErrors() {
				t.Fatalf("Validate() findings = %v, want errors", findings)
			}
		})
	}
}

func TestValidateRejectsClustersWithManagedShortOptions(t *testing.T) {
	tests := []struct {
		name string
		arg  string
	}{
		{name: "output", arg: "-vr/tmp/dump"},
		{name: "host", arg: "-vhdb.other"},
		{name: "port", arg: "-vP3307"},
		{name: "user", arg: "-vuother"},
		{name: "password", arg: "-vpsecret"},
		{name: "all databases", arg: "-vA"},
		{name: "databases", arg: "-vB"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			job := cfg.Jobs["production"]
			job.MySQL.ExtraArgs = []string{tt.arg}
			cfg.Jobs["production"] = job
			if findings := Validate(cfg); !findings.HasErrors() {
				t.Fatalf("Validate() findings = %v, want error for %q", findings, tt.arg)
			}
		})
	}
}

func TestValidateWarnsForPlainHTTPEndpoint(t *testing.T) {
	cfg := validConfig()
	job := cfg.Jobs["production"]
	job.S3.Endpoint = "http://127.0.0.1:9000"
	cfg.Jobs["production"] = job
	findings := Validate(cfg)
	if !findings.ContainsWarning("jobs.production.s3.endpoint") {
		t.Fatalf("findings = %v, want HTTP endpoint warning", findings)
	}
	if findings.HasErrors() {
		t.Fatalf("unexpected errors: %v", findings)
	}
}

func TestValidateRotationConversionBounds(t *testing.T) {
	maxSizeMB := int(math.MaxInt64 / (1024 * 1024))
	maxAgeDays := int(math.MaxInt64 / int64(24*time.Hour))
	for _, tt := range []struct {
		name     string
		sizeMB   int
		ageDays  int
		wantPath string
	}{
		{name: "maximum size", sizeMB: maxSizeMB, ageDays: 1},
		{name: "maximum age", sizeMB: 1, ageDays: maxAgeDays},
		{name: "size overflow", sizeMB: maxSizeMB + 1, ageDays: 1, wantPath: "logging.rotation.max_size_mb"},
		{name: "age overflow", sizeMB: 1, ageDays: maxAgeDays + 1, wantPath: "logging.rotation.max_age_days"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Logging.Rotation.MaxSizeMB = tt.sizeMB
			cfg.Logging.Rotation.MaxAgeDays = tt.ageDays
			findings := Validate(cfg)
			if tt.wantPath == "" {
				if findings.HasErrors() {
					t.Fatalf("Validate() findings = %v, want boundary accepted", findings)
				}
				return
			}
			found := false
			for _, finding := range findings {
				if finding.Path == tt.wantPath {
					found = true
				}
			}
			if !found {
				t.Fatalf("Validate() findings = %v, want %s overflow", findings, tt.wantPath)
			}
		})
	}
}
