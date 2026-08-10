package config

import (
	"fmt"
	"math"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ezgamehost/ezdbbackup/internal/mysqldumpopt"
)

var (
	jobNamePattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	awsBucketPattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	customBucketPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{1,61}[A-Za-z0-9]$`)
	regionPattern        = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	domainLabelPattern   = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
	reservedBucketPrefix = []string{"xn--", "sthree-", "amzn-s3-demo-"}
	reservedBucketSuffix = []string{"-s3alias", "--ol-s3", ".mrap", "--x-s3", "--table-s3"}
)

const (
	maxRotationSizeMB  = math.MaxInt64 / (1024 * 1024)
	maxRotationAgeDays = math.MaxInt64 / int64(24*time.Hour)
	maxRotationFiles   = 1000
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
	} else if cfg.Logging.Rotation.MaxFiles > maxRotationFiles {
		findings.addError("logging.rotation.max_files", fmt.Sprintf("must not exceed %d", maxRotationFiles))
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
	} else {
		validateBucket(findings, name, base+".s3.bucket", job.S3.Bucket, job.S3.Endpoint != "")
	}
	if strings.TrimSpace(job.S3.Region) == "" {
		findings.addJobError(name, base+".s3.region", "is required")
	} else if len(job.S3.Region) > 63 || !regionPattern.MatchString(job.S3.Region) {
		findings.addJobError(name, base+".s3.region", "must be a lowercase region identifier of at most 63 characters")
	}
	validateEndpoint(findings, name, base+".s3.endpoint", job.S3.Endpoint)
	if !validS3Prefix(job.S3.Prefix, name) {
		findings.addJobError(name, base+".s3.prefix", "must form a terminal-safe S3 key of at most 1024 bytes without relative path segments")
	}
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

func validS3Prefix(prefix, jobName string) bool {
	if !utf8.ValidString(prefix) || strings.IndexFunc(prefix, unsafeS3TextRune) >= 0 {
		return false
	}
	segments := strings.FieldsFunc(prefix, func(r rune) bool { return r == '/' })
	for _, segment := range segments {
		if segment == "." || segment == ".." {
			return false
		}
	}
	normalized := strings.Join(segments, "/")
	// ObjectKey appends job/YYYY/MM/DD/job-<26-byte timestamp>.sql.gz.
	keyLength := 2*len(jobName) + 46
	if normalized != "" {
		keyLength += len(normalized) + 1
	}
	return keyLength <= 1024
}

func unsafeS3TextRune(value rune) bool {
	return unicode.IsControl(value) || unicode.In(value, unicode.Cf, unicode.Zl, unicode.Zp)
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
	if err != nil || !validEndpointURL(endpoint, u) {
		findings.addJobError(job, path, "must be an http:// or https:// URL with a valid host and no userinfo, query, fragment, or control characters")
		return
	}
	if u.Scheme == "http" {
		findings.addJobWarning(job, path, "plain HTTP endpoint configured")
	}
}

func validateBucket(findings *Findings, job, path, bucket string, customEndpoint bool) {
	if customEndpoint {
		if !customBucketPattern.MatchString(bucket) || strings.Contains(bucket, "..") ||
			strings.Contains(bucket, ".-") || strings.Contains(bucket, "-.") {
			findings.addJobError(job, path, "must be a single 3-63 character compatible bucket name")
		}
		return
	}
	if !validAWSBucket(bucket) {
		findings.addJobError(job, path, "must be a valid AWS S3 bucket name")
	}
}

func validAWSBucket(bucket string) bool {
	if !awsBucketPattern.MatchString(bucket) || strings.Contains(bucket, "..") ||
		strings.Contains(bucket, ".-") || strings.Contains(bucket, "-.") || net.ParseIP(bucket) != nil {
		return false
	}
	for _, prefix := range reservedBucketPrefix {
		if strings.HasPrefix(bucket, prefix) {
			return false
		}
	}
	for _, suffix := range reservedBucketSuffix {
		if strings.HasSuffix(bucket, suffix) {
			return false
		}
	}
	return true
}

func validEndpointURL(raw string, u *url.URL) bool {
	if u == nil || !utf8.ValidString(raw) || strings.IndexFunc(raw, unicode.IsControl) >= 0 ||
		strings.Contains(raw, "#") ||
		u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return false
	}
	decodedPath, err := url.PathUnescape(u.EscapedPath())
	if err != nil || !utf8.ValidString(decodedPath) || strings.IndexFunc(decodedPath, unicode.IsControl) >= 0 {
		return false
	}
	host := u.Hostname()
	if host == "" || !validEndpointHost(host) {
		return false
	}
	if strings.HasSuffix(u.Host, ":") {
		return false
	}
	if port := u.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return false
		}
	}
	return true
}

func validEndpointHost(host string) bool {
	if net.ParseIP(host) != nil {
		return true
	}
	host = strings.TrimSuffix(host, ".")
	if host == "" || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if !domainLabelPattern.MatchString(label) {
			return false
		}
	}
	return true
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
