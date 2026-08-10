package config

import (
	"math"
	"strings"
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

// This fails if spelling variants, parser controls, credential options,
// side-output options, or live-server mutations cross the extra_args boundary.
func TestValidateRejectsUnsafeExtraArguments(t *testing.T) {
	tests := []struct {
		name string
		arg  string
	}{
		{name: "parser terminator", arg: "--"},
		{name: "positional", arg: "otherdb"},
		{name: "empty positional", arg: ""},
		{name: "short quick", arg: "-q"},
		{name: "short verbose", arg: "-v"},
		{name: "case insensitive host", arg: "--HOST=other"},
		{name: "owned port", arg: "--port=3307"},
		{name: "owned user", arg: "--user=other"},
		{name: "password", arg: "--password=secret"},
		{name: "first factor password", arg: "--password1=secret"},
		{name: "second factor password", arg: "--password2=secret"},
		{name: "third factor password", arg: "--password3=secret"},
		{name: "future factor password", arg: "--password9=secret"},
		{name: "skip password", arg: "--skip-password"},
		{name: "canonicalized skip factor password", arg: "--SKIP_PASSWORD2"},
		{name: "result file", arg: "--result-file=/tmp/plain.sql"},
		{name: "canonicalized result file", arg: "--RESULT_FILE=/tmp/plain.sql"},
		{name: "tab directory", arg: "--tab=/tmp/plain"},
		{name: "parallel output directory", arg: "--dir=/tmp/plain"},
		{name: "canonicalized parallel output directory", arg: "--DIR=/tmp/plain"},
		{name: "output alias", arg: "--output=/tmp/plain.sql"},
		{name: "error side output", arg: "--log-error=/tmp/dump.log"},
		{name: "debug side output", arg: "--debug=d:t:o,/tmp/mysql.trace"},
		{name: "all databases", arg: "--all-databases"},
		{name: "canonicalized all databases", arg: "--ALL_DATABASES"},
		{name: "databases", arg: "--databases"},
		{name: "tables override", arg: "--tables"},
		{name: "database wildcards", arg: "--wildcards"},
		{name: "help", arg: "--help"},
		{name: "version", arg: "--version"},
		{name: "print defaults", arg: "--print-defaults"},
		{name: "defaults file", arg: "--defaults-file=/tmp/my.cnf"},
		{name: "defaults extra file", arg: "--defaults-extra-file=/tmp/my.cnf"},
		{name: "defaults group suffix", arg: "--defaults-group-suffix=other"},
		{name: "no defaults", arg: "--no-defaults"},
		{name: "login path", arg: "--login-path=other"},
		{name: "no login paths", arg: "--no-login-paths"},
		{name: "delete source logs", arg: "--delete-source-logs"},
		{name: "canonicalized delete source logs", arg: "--DELETE_SOURCE_LOGS"},
		{name: "delete master logs", arg: "--delete-master-logs"},
		{name: "flush logs", arg: "--flush-logs"},
		{name: "flush server privileges", arg: "--flush-privileges"},
		{name: "stop replica", arg: "--dump-replica=2"},
		{name: "stop replica legacy alias", arg: "--DUMP_SLAVE"},
		{name: "arbitrary SQL on connect", arg: "--init-command=DELETE FROM audit"},
		{name: "additional arbitrary SQL on connect", arg: "--init-command-add=DELETE FROM audit"},
		{name: "loose modifier bypass", arg: "--loose-delete-source-logs"},
		{name: "nested loose credential bypass", arg: "--enable-loose-password=secret"},
		{name: "nested maximum loose side output bypass", arg: "--maximum-loose-result-file=/tmp/plain.sql"},
		{name: "nested skip loose server mutation bypass", arg: "--skip-loose-delete-source-logs"},
		{name: "abbreviated result file", arg: "--res=/tmp/plain.sql"},
		{name: "abbreviated log deletion", arg: "--delete-s"},
		{name: "malformed triple dash", arg: "---result-file=/tmp/plain.sql"},
		{name: "control character", arg: "--where=id\nOR 1=1"},
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

// This fails if normal dump-shaping and TLS options are accidentally rejected
// while hardening the managed-option boundary.
func TestValidateAllowsOrdinaryLongExtraArguments(t *testing.T) {
	cfg := validConfig()
	job := cfg.Jobs["production"]
	job.MySQL.ExtraArgs = []string{
		"--all",
		"--single-transaction",
		"--quick",
		"--ssl-mode=VERIFY_IDENTITY",
		"--where=id > 100",
	}
	cfg.Jobs["production"] = job
	if findings := Validate(cfg); findings.HasErrors() {
		t.Fatalf("Validate() findings = %v, want safe long options accepted", findings)
	}
}

// This fails if database operands can be parsed as options or carry terminal
// control characters into the child-process boundary.
func TestValidateRejectsUnsafeDatabaseNames(t *testing.T) {
	for _, name := range []string{"-application", "application\narchive", "application\tarchive", "application\x00archive"} {
		t.Run(name, func(t *testing.T) {
			cfg := validConfig()
			job := cfg.Jobs["production"]
			job.MySQL.Databases = DatabaseSelection{Names: []string{name}}
			cfg.Jobs["production"] = job
			if findings := Validate(cfg); !findings.HasErrors() {
				t.Fatalf("Validate() findings = %v, want unsafe database-name error", findings)
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

// This fails if AWS bucket names that cannot be addressed safely are allowed
// to reach the SDK's standard S3 endpoint resolver.
func TestValidateS3BucketGrammarForAWS(t *testing.T) {
	for _, bucket := range []string{
		"ab",
		"Uppercase",
		"under_score",
		"192.168.1.1",
		"adjacent..dots",
		"dot.-hyphen",
		"xn--reserved",
		"reserved-s3alias",
	} {
		t.Run(bucket, func(t *testing.T) {
			cfg := validConfig()
			job := cfg.Jobs["production"]
			job.S3.Bucket = bucket
			cfg.Jobs["production"] = job
			if findings := Validate(cfg); !findings.HasErrors() {
				t.Fatalf("Validate() findings = %v, want invalid AWS bucket %q rejected", findings, bucket)
			}
		})
	}

	cfg := validConfig()
	job := cfg.Jobs["production"]
	job.S3.Bucket = "daily.backups-2026"
	cfg.Jobs["production"] = job
	if findings := Validate(cfg); findings.HasErrors() {
		t.Fatalf("Validate() findings = %v, want valid AWS bucket accepted", findings)
	}
}

// This fails if custom endpoints permit bucket text that can escape a path or
// corrupt terminal/log output. Compatible services may use uppercase and
// underscore, but the value remains one bounded bucket segment.
func TestValidateS3BucketGrammarForCustomEndpoint(t *testing.T) {
	for _, bucket := range []string{"ab", ".hidden", "trailing-", "bad/bucket", `bad\bucket`, "bad\nbucket"} {
		t.Run(bucket, func(t *testing.T) {
			cfg := validConfig()
			job := cfg.Jobs["production"]
			job.S3.Endpoint = "https://objects.example.test/base"
			job.S3.Bucket = bucket
			cfg.Jobs["production"] = job
			if findings := Validate(cfg); !findings.HasErrors() {
				t.Fatalf("Validate() findings = %v, want unsafe custom bucket %q rejected", findings, bucket)
			}
		})
	}

	cfg := validConfig()
	job := cfg.Jobs["production"]
	job.S3.Endpoint = "https://objects.example.test/base"
	job.S3.Bucket = "Backup_01"
	cfg.Jobs["production"] = job
	if findings := Validate(cfg); findings.HasErrors() {
		t.Fatalf("Validate() findings = %v, want conservative compatible bucket accepted", findings)
	}
}

// This fails if endpoint credentials, query material, fragments, malformed
// hosts, or percent-encoded controls can cross the SDK/terminal boundary.
func TestValidateS3EndpointStructure(t *testing.T) {
	invalid := []string{
		"https://user:password@objects.example.test/base",
		"https://objects.example.test/base?session=secret",
		"https://objects.example.test/base#fragment",
		"https://objects.example.test/%0aheader",
		"https://objects.example.test:70000/base",
		"https://under_score.example.test/base",
		"https://objects.example.test/base\nnext",
	}
	for _, endpoint := range invalid {
		t.Run(endpoint, func(t *testing.T) {
			cfg := validConfig()
			job := cfg.Jobs["production"]
			job.S3.Endpoint = endpoint
			cfg.Jobs["production"] = job
			if findings := Validate(cfg); !findings.HasErrors() {
				t.Fatalf("Validate() findings = %v, want unsafe endpoint rejected", findings)
			}
		})
	}

	for _, endpoint := range []string{"https://objects.example.test/base/path", "http://127.0.0.1:9000/s3", "http://[::1]:9000/s3"} {
		t.Run(endpoint, func(t *testing.T) {
			cfg := validConfig()
			job := cfg.Jobs["production"]
			job.S3.Endpoint = endpoint
			cfg.Jobs["production"] = job
			if findings := Validate(cfg); findings.HasErrors() {
				t.Fatalf("Validate() findings = %v, want endpoint path prefix preserved", findings)
			}
		})
	}
}

// This fails if regions or prefixes can inject controls or invalid byte
// sequences into signing, object keys, or human-readable findings.
func TestValidateS3RegionAndPrefixStructure(t *testing.T) {
	for _, region := range []string{"-us-east-1", "us-east-1-", "us--east-1", "US-EAST-1", "us/east/1", "us-east-1\nnext", strings.Repeat("a", 64)} {
		t.Run("region_"+region, func(t *testing.T) {
			cfg := validConfig()
			job := cfg.Jobs["production"]
			job.S3.Region = region
			cfg.Jobs["production"] = job
			if findings := Validate(cfg); !findings.HasErrors() {
				t.Fatalf("Validate() findings = %v, want invalid region rejected", findings)
			}
		})
	}

	for _, prefix := range []string{
		"daily\nmysql",
		"daily\x00mysql",
		"daily/\u2028mysql",
		"daily/\u202emysql",
		"../daily",
		"daily/./mysql",
		strings.Repeat("a", 1000),
		string([]byte{0xff, 'x'}),
	} {
		t.Run("prefix", func(t *testing.T) {
			cfg := validConfig()
			job := cfg.Jobs["production"]
			job.S3.Prefix = prefix
			cfg.Jobs["production"] = job
			if findings := Validate(cfg); !findings.HasErrors() {
				t.Fatalf("Validate() findings = %v, want unsafe prefix rejected", findings)
			}
		})
	}

	cfg := validConfig()
	job := cfg.Jobs["production"]
	job.S3.Region = "us-gov-west-1"
	job.S3.Prefix = "production/数据库"
	cfg.Jobs["production"] = job
	if findings := Validate(cfg); findings.HasErrors() {
		t.Fatalf("Validate() findings = %v, want valid region and UTF-8 prefix accepted", findings)
	}
}

// This fails if validation reflects attacker-controlled S3 text into a
// terminal diagnostic instead of returning a fixed structural description.
func TestValidateS3FindingsDoNotEchoUnsafeValues(t *testing.T) {
	const marker = "TERMINAL_SECRET"
	cfg := validConfig()
	job := cfg.Jobs["production"]
	job.S3.Bucket = marker + "\nnext"
	job.S3.Region = marker + "\tregion"
	job.S3.Endpoint = "https://objects.example.test/base?token=" + marker
	job.S3.Prefix = marker + "\rprefix"
	cfg.Jobs["production"] = job

	text := Validate(cfg).Error()
	if strings.Contains(text, marker) || strings.ContainsAny(text, "\r\n\t") {
		t.Fatalf("Validate() findings exposed unsafe S3 input: %q", text)
	}
}

// This fails if downstream validation must infer a job from a dotted path,
// which is ambiguous when malformed names such as "a.bad" are present.
func TestValidateFindingsCarryExactJobIdentity(t *testing.T) {
	cfg := validConfig()
	job := cfg.Jobs["production"]
	delete(cfg.Jobs, "production")
	job.MySQL.Host = ""
	cfg.Jobs["a.bad"] = job

	findings := Validate(cfg)
	foundHost := false
	for _, finding := range findings {
		if finding.Path != "jobs.a.bad.mysql.host" {
			continue
		}
		foundHost = true
		if finding.Job != "a.bad" {
			t.Fatalf("host finding job = %q, want exact dotted job identity", finding.Job)
		}
	}
	if !foundHost {
		t.Fatalf("Validate() findings = %v, want host finding", findings)
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

func TestValidateRejectsUnreasonablyLargeRotationHistory(t *testing.T) {
	cfg := validConfig()
	cfg.Logging.Rotation.MaxFiles = 1001

	findings := Validate(cfg)
	found := false
	for _, finding := range findings {
		if !finding.Warning && finding.Path == "logging.rotation.max_files" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Validate() findings = %v, want max_files upper-bound error", findings)
	}
}

// This fails if schedule validation follows a library dialect instead of the
// numeric/name list-range-step grammar accepted by /etc/cron.d.
func TestValidateCronDDialect(t *testing.T) {
	valid := []string{
		"0 2 * * 0",
		"0 2 * * 7",
		"*/15 0-23/2 1,15 * 1-5",
		"*/15,7 0-23/2 1,15 * 1-5",
		"0,15,30,45 0-23/2 1-31/3 JAN,MAR,DEC SUN,MON-FRI",
		"5 4 * jan sun",
	}
	for _, schedule := range valid {
		t.Run("valid_"+schedule, func(t *testing.T) {
			cfg := validConfig()
			job := cfg.Jobs["production"]
			job.Schedule = schedule
			cfg.Jobs["production"] = job
			if findings := Validate(cfg); findings.HasErrors() {
				t.Fatalf("Validate() findings = %v, want /etc/cron.d schedule accepted", findings)
			}
		})
	}

	invalid := []string{
		"0 2 ? * *",
		"60 2 * * *",
		"0 24 * * *",
		"0 2 0 * *",
		"0 2 32 * *",
		"0 2 * 0 *",
		"0 2 * 13 *",
		"0 2 * * 8",
		"*/0 2 * * *",
		"5-1 2 * * *",
		"1,,2 2 * * *",
		"1- 2 * * *",
		"1/2 2 * * *",
		"0 2 * FOO *",
		"0 2 * * MON#2",
		"0 2 * * MON\n",
	}
	for _, schedule := range invalid {
		t.Run("invalid_"+schedule, func(t *testing.T) {
			cfg := validConfig()
			job := cfg.Jobs["production"]
			job.Schedule = schedule
			cfg.Jobs["production"] = job
			if findings := Validate(cfg); !findings.HasErrors() {
				t.Fatalf("Validate() findings = %v, want /etc/cron.d schedule rejected", findings)
			}
		})
	}
}
