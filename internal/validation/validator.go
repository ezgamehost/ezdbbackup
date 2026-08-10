package validation

import (
	"context"
	"fmt"
	"os/user"
	"sort"
	"strings"

	"github.com/ezgamehost/ezdbbackup/internal/config"
	"github.com/ezgamehost/ezdbbackup/internal/dump"
	"github.com/ezgamehost/ezdbbackup/internal/jobresolve"
	"github.com/ezgamehost/ezdbbackup/internal/storage"
)

// Options controls checks that are specific to the current CLI invocation.
type Options struct {
	Connectivity    bool
	BackupExecution bool
	BinaryPath      string
	ConfigPath      string
}

// Checker performs side-effect-free validation.
type Checker interface {
	Check(context.Context, *config.Config, []string, Options) Report
}

// CurrentUserProvider supplies the invoking user identity for connectivity
// diagnostics.
type CurrentUserProvider interface {
	CurrentUsername() (string, error)
}

// OSCurrentUser reads the user identity of the process invoking validation.
type OSCurrentUser struct{}

func (OSCurrentUser) CurrentUsername() (string, error) {
	current, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("lookup invoking user: %w", err)
	}
	return current.Username, nil
}

// Validator coordinates pure configuration checks, local environment checks,
// and optional read-only connectivity probes. Executable version checks use
// the configured run_as identity when validation is invoked by root.
// Connectivity probes remain in the invoking process identity.
type Validator struct {
	Environment Environment
	Resolve     jobresolve.OptionsResolver
	Dump        dump.Runner
	Stores      storage.Factory
	CurrentUser CurrentUserProvider
}

func (v Validator) Check(ctx context.Context, cfg *config.Config, jobNames []string, options Options) Report {
	names, selected, selectionReport := selectedJobs(cfg, jobNames)
	report := selectionReport
	prerequisites := make(map[string]*jobPrerequisites, len(names))
	for _, name := range names {
		prerequisites[name] = &jobPrerequisites{mysqlConfig: true, s3Config: true, mysqlLocal: true, s3Local: true}
	}

	for _, finding := range config.Validate(cfg) {
		job := finding.Job
		if job != "" && !selected[job] {
			continue
		}
		severity := SeverityError
		if finding.Warning {
			severity = SeverityWarning
		}
		message := finding.Message
		if finding.Path != "" {
			message = finding.Path + ": " + message
		}
		report = report.Append(Finding{Severity: severity, Job: job, Check: "configuration", Message: message})
		if severity == SeverityError && job != "" {
			markConfigPrerequisite(prerequisites[job], finding.Path, job)
		}
	}
	if cfg == nil {
		return report
	}

	if v.Environment == nil {
		return report.Append(Finding{Severity: SeverityError, Check: "environment", Message: "local environment checker is not configured"})
	}
	report = appendCheck(report, "", "cron_binary_path", "binary path is unsafe for cron", v.Environment.CheckCronPath(options.BinaryPath))
	report = appendCheck(report, "", "cron_config_path", "configuration path is unsafe for cron", v.Environment.CheckCronPath(options.ConfigPath))
	logRunAs := make([]string, 0, len(cfg.Jobs))
	seenLogRunAs := make(map[string]bool)
	for _, enabledName := range cfg.EnabledJobNames() {
		runAs := cfg.Jobs[enabledName].RunAs
		if !seenLogRunAs[runAs] {
			seenLogRunAs[runAs] = true
			logRunAs = append(logRunAs, runAs)
		}
	}

	for _, name := range names {
		job := cfg.Jobs[name]
		state := prerequisites[name]

		if err := v.Environment.CheckUser(job.RunAs); err != nil {
			report = appendCheck(report, name, "run_as_user", "configured run_as user is unavailable", err)
			state.mysqlLocal = false
			state.s3Local = false
		}
		if options.BackupExecution {
			if err := v.Environment.CheckRunIdentity(job.RunAs); err != nil {
				report = appendCheck(report, name, "execution_identity", "backup process identity does not match configured run_as", err)
				state.mysqlLocal = false
				state.s3Local = false
			}
		}
		if job.Enabled {
			if err := v.Environment.CheckRuntimeExecutable(ctx, options.BinaryPath, job.RunAs); err != nil {
				report = appendCheck(report, name, "runtime_executable", "scheduled ezdbbackup executable is unavailable or unsafe", err)
				state.mysqlLocal = false
				state.s3Local = false
			}
			if err := v.Environment.CheckConfigFile(options.ConfigPath, job.RunAs); err != nil {
				report = appendCheck(report, name, "configuration_file", "configuration file is unreadable or unsafe for scheduled execution", err)
				state.mysqlLocal = false
				state.s3Local = false
			}
		}
		if err := checkExecutable(ctx, v.Environment, job.DumpBinary, job.RunAs); err != nil {
			report = appendCheck(report, name, "dump_executable", "dump executable is unavailable", err)
			state.mysqlLocal = false
		}
		report = appendCheck(report, name, "temp_directory", "temporary directory is not writable or safe for staging", checkStagingTarget(v.Environment, job.TempDir, job.RunAs))

		if job.MySQL.PasswordFile != "" {
			if err := v.Environment.CheckSecretFile(job.MySQL.PasswordFile, job.RunAs); err != nil {
				report = appendCheck(report, name, "mysql_password_file", "MySQL password file is invalid", err)
				state.mysqlLocal = false
			}
		}
		for _, secret := range s3SecretFiles(job.S3) {
			if secret.path == "" {
				continue
			}
			if err := v.Environment.CheckSecretFile(secret.path, job.RunAs); err != nil {
				report = appendCheck(report, name, secret.check, secret.message, err)
				state.s3Local = false
			}
		}

	}
	if len(logRunAs) > 0 {
		report = appendCheck(report, "", "log_directory", "global log directory is incompatible with enabled run_as identities", v.Environment.CheckLoggingTarget(cfg.Logging.Directory, logRunAs))
	}
	if options.Connectivity {
		currentUser := v.CurrentUser
		if currentUser == nil {
			currentUser = OSCurrentUser{}
		}
		invokingUser, currentUserErr := currentUser.CurrentUsername()
		for _, name := range names {
			job := cfg.Jobs[name]
			if currentUserErr != nil {
				report = report.Append(Finding{
					Severity: SeverityWarning,
					Job:      name,
					Check:    "connectivity_identity",
					Message:  fmt.Sprintf("could not determine invoking user: %v; connectivity probes still use the invoking process, not configured run_as user %q", currentUserErr, job.RunAs),
				})
			} else if invokingUser != job.RunAs {
				report = report.Append(connectivityIdentityWarning(name, invokingUser, job.RunAs))
			}
			report = v.checkConnectivity(ctx, report, name, job, *prerequisites[name])
		}
	}
	return report
}

func connectivityIdentityWarning(job, invokingUser, runAs string) Finding {
	return Finding{
		Severity: SeverityWarning,
		Job:      job,
		Check:    "connectivity_identity",
		Message: fmt.Sprintf(
			"connectivity probes use invoking user %q, not configured run_as user %q; to test with the scheduled-job identity, run `sudo -u %s ezdbbackup validate --connectivity`",
			invokingUser,
			runAs,
			runAs,
		),
	}
}

func checkExecutable(ctx context.Context, environment Environment, path, runAs string) error {
	if runAsEnvironment, ok := environment.(runAsExecutableEnvironment); ok {
		return runAsEnvironment.CheckExecutableAs(ctx, path, runAs)
	}
	return environment.CheckExecutable(ctx, path)
}

func checkStagingTarget(environment Environment, path, runAs string) error {
	if stagingEnvironment, ok := environment.(stagingTargetEnvironment); ok {
		return stagingEnvironment.CheckStagingTarget(path, runAs)
	}
	return environment.CheckWritableTarget(path, runAs)
}

type jobPrerequisites struct {
	mysqlConfig bool
	s3Config    bool
	mysqlLocal  bool
	s3Local     bool
}

func (v Validator) checkConnectivity(ctx context.Context, report Report, name string, job config.JobConfig, state jobPrerequisites) Report {
	secrets := configuredSecretValues(job)
	if !state.mysqlConfig {
		report = report.Append(skippedFinding(name, "mysql_connectivity", "skipped because MySQL configuration prerequisites failed"))
	} else if !state.mysqlLocal {
		report = report.Append(skippedFinding(name, "mysql_connectivity", "skipped because local MySQL prerequisites failed"))
	} else if v.Resolve == nil || v.Dump == nil {
		report = report.Append(Finding{Severity: SeverityError, Job: name, Check: "mysql_connectivity", Message: "MySQL connectivity dependencies are not configured"})
	} else {
		request, err := v.Resolve.Dump(job)
		if err != nil {
			report = appendCheck(report, name, "mysql_connectivity", "resolve MySQL probe options", redactCause(err, secrets...))
		} else {
			secrets = appendNonEmpty(secrets, request.Password)
			report = appendCheck(report, name, "mysql_connectivity", "MySQL connection probe failed", redactCause(v.Dump.Probe(ctx, request), secrets...))
		}
	}

	if !state.s3Config {
		return report.Append(skippedFinding(name, "s3_connectivity", "skipped because S3 configuration prerequisites failed"))
	}
	if !state.s3Local {
		return report.Append(skippedFinding(name, "s3_connectivity", "skipped because local S3 prerequisites failed"))
	}
	if v.Resolve == nil || v.Stores == nil {
		return report.Append(Finding{Severity: SeverityError, Job: name, Check: "s3_connectivity", Message: "S3 connectivity dependencies are not configured"})
	}
	storeOptions, err := v.Resolve.Storage(job)
	if err != nil {
		return appendCheck(report, name, "s3_connectivity", "resolve S3 client options", redactCause(err, secrets...))
	}
	secrets = appendNonEmpty(
		secrets,
		storeOptions.Credentials.AccessKeyID,
		storeOptions.Credentials.SecretAccessKey,
		storeOptions.Credentials.SessionToken,
	)
	store, err := v.Stores.New(ctx, storeOptions)
	if err != nil {
		return appendCheck(report, name, "s3_connectivity", "create S3 client", redactCause(err, secrets...))
	}
	return appendCheck(
		report,
		name,
		"s3_connectivity",
		"S3 bucket inspection failed; bucket inspection may be denied even when uploads are allowed",
		redactCause(store.Probe(ctx, job.S3.Bucket), secrets...),
	)
}

func configuredSecretValues(job config.JobConfig) []string {
	return appendNonEmpty(
		nil,
		job.MySQL.Password,
		job.S3.AccessKeyID,
		job.S3.SecretAccessKey,
		job.S3.SessionToken,
	)
}

func selectedJobs(cfg *config.Config, requested []string) ([]string, map[string]bool, Report) {
	selected := make(map[string]bool)
	if cfg == nil {
		return nil, selected, Report{}
	}
	if len(requested) == 0 {
		names := cfg.JobNames()
		for _, name := range names {
			selected[name] = true
		}
		return names, selected, Report{}
	}

	names := make([]string, 0, len(requested))
	report := Report{}
	for _, name := range requested {
		if selected[name] {
			continue
		}
		selected[name] = true
		if _, ok := cfg.Jobs[name]; !ok {
			report = report.Append(Finding{
				Severity: SeverityError,
				Job:      name,
				Check:    "selection",
				Message:  fmt.Sprintf("job %q is not configured", name),
			})
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, selected, report
}

func markConfigPrerequisite(state *jobPrerequisites, path, job string) {
	if state == nil {
		return
	}
	relative := strings.TrimPrefix(path, "jobs."+job+".")
	switch {
	case relative == "run_as":
		state.mysqlConfig = false
		state.s3Config = false
	case relative == "dump_binary" || strings.HasPrefix(relative, "mysql."):
		state.mysqlConfig = false
	case strings.HasPrefix(relative, "s3.") || relative == "s3":
		state.s3Config = false
	}
}

type secretFileCheck struct {
	path    string
	check   string
	message string
}

func s3SecretFiles(cfg config.S3Config) []secretFileCheck {
	return []secretFileCheck{
		{path: cfg.AccessKeyIDFile, check: "s3_access_key_id_file", message: "S3 access key ID file is invalid"},
		{path: cfg.SecretAccessKeyFile, check: "s3_secret_access_key_file", message: "S3 secret access key file is invalid"},
		{path: cfg.SessionTokenFile, check: "s3_session_token_file", message: "S3 session token file is invalid"},
	}
}

func skippedFinding(job, check, message string) Finding {
	return Finding{Severity: SeverityWarning, Job: job, Check: check, Message: message}
}

func appendCheck(report Report, job, check, message string, err error) Report {
	if err == nil {
		return report
	}
	return report.Append(Finding{Severity: SeverityError, Job: job, Check: check, Message: message, Cause: err})
}
