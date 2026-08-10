package config

import (
	"fmt"
	"math"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/ezgamehost/ezdbbackup/internal/mysqldumpopt"
)

var jobNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

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
	validateAbsolutePath(&findings, "", "defaults.dump_binary", cfg.Defaults.DumpBinary)
	validateAbsolutePath(&findings, "", "defaults.temp_dir", cfg.Defaults.TempDir)
	validateAbsolutePath(&findings, "", "logging.directory", cfg.Logging.Directory)
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
		findings.addJobError(name, base, "job name must match [A-Za-z0-9][A-Za-z0-9_-]{0,63}")
	}
	if err := ValidateCronSchedule(job.Schedule); err != nil {
		findings.addJobError(name, base+".schedule", fmt.Sprintf("invalid cron expression: %v", err))
	}
	if strings.TrimSpace(job.RunAs) == "" {
		findings.addJobError(name, base+".run_as", "is required")
	}
	validateAbsolutePath(findings, name, base+".dump_binary", job.DumpBinary)
	validateAbsolutePath(findings, name, base+".temp_dir", job.TempDir)

	if strings.TrimSpace(job.MySQL.Host) == "" {
		findings.addJobError(name, base+".mysql.host", "is required")
	}
	if job.MySQL.Port < 1 || job.MySQL.Port > 65535 {
		findings.addJobError(name, base+".mysql.port", "must be between 1 and 65535")
	}
	if strings.TrimSpace(job.MySQL.User) == "" {
		findings.addJobError(name, base+".mysql.user", "is required")
	}
	databases := job.MySQL.Databases
	if databases.All && len(databases.Names) > 0 {
		findings.addJobError(name, base+".mysql.databases", "must select either all databases or an explicit list")
	} else if !databases.All && len(databases.Names) == 0 {
		findings.addJobError(name, base+".mysql.databases", "is required")
	} else if !databases.All {
		for i, databaseName := range databases.Names {
			if databaseName == "" {
				findings.addJobError(name, fmt.Sprintf("%s.mysql.databases[%d]", base, i), "must not be empty")
			} else if strings.HasPrefix(databaseName, "-") {
				findings.addJobError(name, fmt.Sprintf("%s.mysql.databases[%d]", base, i), "must not begin with '-'")
			} else if strings.IndexFunc(databaseName, unicode.IsControl) >= 0 {
				findings.addJobError(name, fmt.Sprintf("%s.mysql.databases[%d]", base, i), "must not contain control characters")
			}
		}
	}
	validateSecretRef(findings, name, base+".mysql.password", job.MySQL.PasswordRef())
	for i, arg := range job.MySQL.ExtraArgs {
		if conflictingDumpArgument(arg) {
			findings.addJobError(name, fmt.Sprintf("%s.mysql.extra_args[%d]", base, i), "conflicts with an ezdbbackup-managed mysqldump setting")
		}
	}

	if strings.TrimSpace(job.S3.Bucket) == "" {
		findings.addJobError(name, base+".s3.bucket", "is required")
	}
	if strings.TrimSpace(job.S3.Region) == "" {
		findings.addJobError(name, base+".s3.region", "is required")
	}
	validateEndpoint(findings, name, base+".s3.endpoint", job.S3.Endpoint)
	accessKeyID := job.S3.AccessKeyIDRef()
	secretAccessKey := job.S3.SecretAccessKeyRef()
	sessionToken := job.S3.SessionTokenRef()
	validateSecretRef(findings, name, base+".s3.access_key_id", accessKeyID)
	validateSecretRef(findings, name, base+".s3.secret_access_key", secretAccessKey)
	validateSecretRef(findings, name, base+".s3.session_token", sessionToken)
	accessConfigured := secretConfigured(accessKeyID)
	secretKeyConfigured := secretConfigured(secretAccessKey)
	if accessConfigured != secretKeyConfigured {
		findings.addJobError(name, base+".s3", "explicit access key ID and secret access key must be configured together")
	}
	if secretConfigured(sessionToken) && (!accessConfigured || !secretKeyConfigured) {
		findings.addJobError(name, base+".s3.session_token", "requires explicit access key ID and secret access key")
	}
}

func validateAbsolutePath(findings *Findings, job, path, value string) {
	if value == "" || !filepath.IsAbs(value) {
		findings.addJobError(job, path, "must be an absolute path")
	}
}

func validateSecretRef(findings *Findings, job, path string, secret SecretRef) {
	if secret.Literal != "" && secret.File != "" {
		findings.addJobError(job, path, "literal and file secret sources are mutually exclusive")
	}
	if secret.File != "" && !filepath.IsAbs(secret.File) {
		findings.addJobError(job, path+"_file", "must be an absolute path")
	}
}

func validateEndpoint(findings *Findings, job, path, endpoint string) {
	if endpoint == "" {
		return
	}
	u, err := url.ParseRequestURI(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		findings.addJobError(job, path, "must be a complete http:// or https:// URL")
		return
	}
	if u.Scheme == "http" {
		findings.addJobWarning(job, path, "plain HTTP endpoint configured")
	}
}

func secretConfigured(secret SecretRef) bool {
	return secret.Literal != "" || secret.File != ""
}

func conflictingDumpArgument(arg string) bool {
	if strings.IndexFunc(arg, unicode.IsControl) >= 0 {
		return true
	}
	name, ok := mysqldumpopt.LongName(arg)
	if !ok {
		return true
	}
	base := name
	for {
		stripped := false
		for _, prefix := range []string{"enable-", "disable-", "skip-", "maximum-", "loose-"} {
			if strings.HasPrefix(base, prefix) {
				if prefix == "loose-" {
					return true
				}
				base = strings.TrimPrefix(base, prefix)
				stripped = true
				break
			}
		}
		if !stripped {
			break
		}
	}
	if _, allowed := safeExactDumpOptions[name]; allowed {
		return false
	}
	if _, forbidden := forbiddenDumpOptions[base]; forbidden {
		return true
	}
	if strings.HasPrefix(base, "password") || forbiddenDumpOptionOrAbbreviation(base) {
		return true
	}
	return strings.HasPrefix(base, "init-command-")
}

var safeExactDumpOptions = map[string]struct{}{
	// MariaDB's --all is a deprecated alias for --create-options, not a
	// database-scope selector. Do not confuse it with --all-databases.
	"all": {},
}

func forbiddenDumpOptionOrAbbreviation(name string) bool {
	for forbidden := range forbiddenDumpOptions {
		if name == forbidden || strings.HasPrefix(forbidden, name) {
			return true
		}
	}
	return false
}

var forbiddenDumpOptions = map[string]struct{}{
	"all-databases":                 {},
	"databases":                     {},
	"debug":                         {},
	"debug-check":                   {},
	"debug-info":                    {},
	"defaults-extra-file":           {},
	"defaults-file":                 {},
	"defaults-group-suffix":         {},
	"delete-master-logs":            {},
	"delete-source-logs":            {},
	"dir":                           {},
	"dump-replica":                  {},
	"dump-slave":                    {},
	"fields-enclosed-by":            {},
	"fields-escaped-by":             {},
	"fields-optionally-enclosed-by": {},
	"fields-terminated-by":          {},
	"flush-logs":                    {},
	"flush-privileges":              {},
	"help":                          {},
	"host":                          {},
	"ignore-database":               {},
	"ignore-table":                  {},
	"ignore-table-data":             {},
	"init-command":                  {},
	"lines-terminated-by":           {},
	"log-error":                     {},
	"login-path":                    {},
	"no-defaults":                   {},
	"no-login-paths":                {},
	"output":                        {},
	"password":                      {},
	"password1":                     {},
	"password2":                     {},
	"password3":                     {},
	"port":                          {},
	"print-defaults":                {},
	"result-file":                   {},
	"system":                        {},
	"tab":                           {},
	"tables":                        {},
	"tee":                           {},
	"user":                          {},
	"users":                         {},
	"version":                       {},
	"wildcards":                     {},
}
