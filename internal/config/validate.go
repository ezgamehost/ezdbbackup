package config

import (
	"fmt"
	"math"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

var jobNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

const (
	maxRotationSizeMB  = math.MaxInt64 / (1024 * 1024)
	maxRotationAgeDays = math.MaxInt64 / int64(24*time.Hour)
)

// Validate performs configuration-only checks without accessing the network,
// filesystem, or local user database.
func Validate(cfg *Config) Findings {
	var findings Findings
	if cfg == nil {
		findings.addError("", "configuration is required")
		return findings
	}
	if cfg.Version != 1 {
		findings.addError("version", "must be 1")
	}
	validateAbsolutePath(&findings, "defaults.dump_binary", cfg.Defaults.DumpBinary)
	validateAbsolutePath(&findings, "defaults.temp_dir", cfg.Defaults.TempDir)
	validateAbsolutePath(&findings, "logging.directory", cfg.Logging.Directory)
	if cfg.Logging.Rotation.MaxSizeMB <= 0 {
		findings.addError("logging.rotation.max_size_mb", "must be greater than zero")
	} else if int64(cfg.Logging.Rotation.MaxSizeMB) > maxRotationSizeMB {
		findings.addError("logging.rotation.max_size_mb", fmt.Sprintf("must not exceed %d", maxRotationSizeMB))
	}
	if cfg.Logging.Rotation.MaxFiles <= 0 {
		findings.addError("logging.rotation.max_files", "must be greater than zero")
	}
	if cfg.Logging.Rotation.MaxAgeDays <= 0 {
		findings.addError("logging.rotation.max_age_days", "must be greater than zero")
	} else if int64(cfg.Logging.Rotation.MaxAgeDays) > maxRotationAgeDays {
		findings.addError("logging.rotation.max_age_days", fmt.Sprintf("must not exceed %d", maxRotationAgeDays))
	}

	for _, name := range cfg.JobNames() {
		validateJob(&findings, name, cfg.Jobs[name])
	}
	return findings
}

func validateJob(findings *Findings, name string, job JobConfig) {
	base := "jobs." + name
	if !jobNamePattern.MatchString(name) {
		findings.addError(base, "job name must match [A-Za-z0-9][A-Za-z0-9_-]{0,63}")
	}
	if strings.TrimSpace(job.Schedule) == "" || len(strings.Fields(job.Schedule)) != 5 {
		findings.addError(base+".schedule", "must be a five-field cron expression")
	} else if _, err := cronParser.Parse(job.Schedule); err != nil {
		findings.addError(base+".schedule", fmt.Sprintf("invalid cron expression: %v", err))
	}
	if strings.TrimSpace(job.RunAs) == "" {
		findings.addError(base+".run_as", "is required")
	}
	validateAbsolutePath(findings, base+".dump_binary", job.DumpBinary)
	validateAbsolutePath(findings, base+".temp_dir", job.TempDir)

	if strings.TrimSpace(job.MySQL.Host) == "" {
		findings.addError(base+".mysql.host", "is required")
	}
	if job.MySQL.Port < 1 || job.MySQL.Port > 65535 {
		findings.addError(base+".mysql.port", "must be between 1 and 65535")
	}
	if strings.TrimSpace(job.MySQL.User) == "" {
		findings.addError(base+".mysql.user", "is required")
	}
	databases := job.MySQL.Databases
	if databases.All && len(databases.Names) > 0 {
		findings.addError(base+".mysql.databases", "must select either all databases or an explicit list")
	} else if !databases.All && len(databases.Names) == 0 {
		findings.addError(base+".mysql.databases", "is required")
	} else if !databases.All {
		for i, name := range databases.Names {
			if name == "" {
				findings.addError(fmt.Sprintf("%s.mysql.databases[%d]", base, i), "must not be empty")
			}
		}
	}
	validateSecretRef(findings, base+".mysql.password", job.MySQL.PasswordRef())
	for i, arg := range job.MySQL.ExtraArgs {
		if conflictingDumpArgument(arg) {
			findings.addError(fmt.Sprintf("%s.mysql.extra_args[%d]", base, i), "conflicts with an ezdbbackup-managed mysqldump setting")
		}
	}

	if strings.TrimSpace(job.S3.Bucket) == "" {
		findings.addError(base+".s3.bucket", "is required")
	}
	if strings.TrimSpace(job.S3.Region) == "" {
		findings.addError(base+".s3.region", "is required")
	}
	validateEndpoint(findings, base+".s3.endpoint", job.S3.Endpoint)
	accessKeyID := job.S3.AccessKeyIDRef()
	secretAccessKey := job.S3.SecretAccessKeyRef()
	sessionToken := job.S3.SessionTokenRef()
	validateSecretRef(findings, base+".s3.access_key_id", accessKeyID)
	validateSecretRef(findings, base+".s3.secret_access_key", secretAccessKey)
	validateSecretRef(findings, base+".s3.session_token", sessionToken)
	accessConfigured := secretConfigured(accessKeyID)
	secretKeyConfigured := secretConfigured(secretAccessKey)
	if accessConfigured != secretKeyConfigured {
		findings.addError(base+".s3", "explicit access key ID and secret access key must be configured together")
	}
	if secretConfigured(sessionToken) && (!accessConfigured || !secretKeyConfigured) {
		findings.addError(base+".s3.session_token", "requires explicit access key ID and secret access key")
	}
}

func validateAbsolutePath(findings *Findings, path, value string) {
	if value == "" || !filepath.IsAbs(value) {
		findings.addError(path, "must be an absolute path")
	}
}

func validateSecretRef(findings *Findings, path string, secret SecretRef) {
	if secret.Literal != "" && secret.File != "" {
		findings.addError(path, "literal and file secret sources are mutually exclusive")
	}
	if secret.File != "" && !filepath.IsAbs(secret.File) {
		findings.addError(path+"_file", "must be an absolute path")
	}
}

func validateEndpoint(findings *Findings, path, endpoint string) {
	if endpoint == "" {
		return
	}
	u, err := url.ParseRequestURI(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		findings.addError(path, "must be a complete http:// or https:// URL")
		return
	}
	if u.Scheme == "http" {
		findings.addWarning(path, "plain HTTP endpoint configured")
	}
}

func secretConfigured(secret SecretRef) bool {
	return secret.Literal != "" || secret.File != ""
}

func conflictingDumpArgument(arg string) bool {
	if !strings.HasPrefix(arg, "-") {
		return true
	}
	for _, option := range []string{
		"--output", "--result-file", "--tab", "--host", "--port", "--user", "--password", "--all-databases", "--databases",
	} {
		if arg == option || strings.HasPrefix(arg, option+"=") {
			return true
		}
	}
	if containsManagedShortOption(arg) {
		return true
	}
	return false
}

func containsManagedShortOption(arg string) bool {
	if !strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "--") {
		return false
	}
	for _, option := range "rhPupAB" {
		if strings.ContainsRune(arg[1:], option) {
			return true
		}
	}
	return false
}
