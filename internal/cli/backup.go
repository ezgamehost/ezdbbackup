package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ezgamehost/ezdbbackup/internal/backup"
	"github.com/ezgamehost/ezdbbackup/internal/config"
	"github.com/ezgamehost/ezdbbackup/internal/validation"
)

func runBackup(ctx context.Context, args []string, deps Dependencies) int {
	flags := flag.NewFlagSet("backup", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", defaultConfigPath, "configuration file")
	debug := flags.Bool("debug", false, "enable debug logging")
	all := flags.Bool("all", false, "back up every enabled job")
	flags.Usage = func() {
		fmt.Fprintln(deps.Stderr, "usage: ezdbbackup backup <job> [--config <path>] [--debug]")
		fmt.Fprintln(deps.Stderr, "       ezdbbackup backup --all [--config <path>] [--debug]")
	}
	if err := parseInterspersed(flags, args, map[string]bool{"config": true}); err != nil {
		fmt.Fprintln(deps.Stderr, encoded(err))
		flags.Usage()
		return 2
	}
	positional := flags.Args()
	if (*all && len(positional) != 0) || (!*all && len(positional) != 1) {
		fmt.Fprintln(deps.Stderr, "backup requires exactly one job or --all")
		flags.Usage()
		return 2
	}

	effectiveConfig, err := effectiveAbsolutePath(*configPath)
	if err != nil {
		fmt.Fprintf(deps.Stderr, "configuration path: %s\n", encoded(err))
		return 2
	}
	cfg, findings := deps.LoadConfig(effectiveConfig)
	if printConfigFindings(findings, deps) {
		return 2
	}
	trustedConfig := cfg.TrustedPath(effectiveConfig)

	var selected []string
	if *all {
		selected = cfg.EnabledJobNames()
	} else {
		name := positional[0]
		job, exists := cfg.Jobs[name]
		if !exists {
			fmt.Fprintf(deps.Stderr, "backup: job %s is not configured\n", encoded(name))
			return 2
		}
		if !job.Enabled {
			fmt.Fprintf(deps.Stderr, "backup: job %s is disabled\n", encoded(name))
			return 2
		}
		selected = []string{name}
	}
	if *all && len(selected) == 0 {
		if printConfigFindings(globalConfigFindings(cfg), deps) {
			return 2
		}
		printBackupSummary(backup.Summary{}, deps)
		return 0
	}

	binaryPath, err := executablePath(deps)
	if err != nil {
		fmt.Fprintf(deps.Stderr, "backup validation: %s\n", encoded(err))
		return 2
	}
	report := deps.Validator.Check(ctx, cfg, selected, validation.Options{
		BackupExecution: true,
		BinaryPath:      binaryPath,
		ConfigPath:      trustedConfig,
	})
	if printValidationReport(report, deps) {
		return 2
	}
	if err := cfg.RecheckSource(); err != nil {
		fmt.Fprintf(deps.Stderr, "backup validation: loaded configuration source changed: %s\n", encoded(err))
		return 2
	}

	options, err := logOptions(cfg.Logging, *debug)
	if err != nil {
		fmt.Fprintf(deps.Stderr, "backup: invalid logging configuration: %s\n", encoded(err))
		return 2
	}
	logger, err := deps.NewLogger(options)
	if err != nil {
		fmt.Fprintf(deps.Stderr, "backup: initialize logging: %s\n", encoded(err))
		return 1
	}
	service := deps.NewBackup(logger)
	service.Debug = options.Debug
	service.Progress = func(event backup.ProgressEvent) {
		printProgress(deps.Stdout, event)
	}
	var names []string
	if !*all {
		names = selected
	}
	summary := service.RunMany(ctx, cfg, names)
	printBackupSummary(summary, deps)
	if summary.HasFailures() {
		return 1
	}
	return 0
}

func globalConfigFindings(cfg *config.Config) config.Findings {
	all := config.Validate(cfg)
	global := make(config.Findings, 0, len(all))
	for _, finding := range all {
		if finding.Job != "" {
			continue
		}
		global = append(global, finding)
	}
	return global
}

func executablePath(deps Dependencies) (string, error) {
	path, err := deps.ExecutablePath()
	if err != nil {
		return "", fmt.Errorf("resolve executable path: %w", err)
	}
	absolute, err := effectiveAbsolutePath(path)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err == nil {
		return filepath.Clean(canonical), nil
	}
	// Unit-injected paths may intentionally be absent; production os.Executable
	// always names the running image and therefore takes the canonical branch.
	if errors.Is(err, os.ErrNotExist) {
		return absolute, nil
	}
	return "", fmt.Errorf("resolve canonical executable path: %w", err)
}

func printConfigFindings(findings config.Findings, deps Dependencies) bool {
	for _, finding := range findings {
		writer := deps.Stderr
		if finding.Warning {
			writer = deps.Stdout
		}
		fmt.Fprintln(writer, encoded(finding.String()))
	}
	return findings.HasErrors()
}

func printValidationReport(report validation.Report, deps Dependencies) bool {
	for _, finding := range report.Findings {
		writer := deps.Stderr
		if finding.Severity == validation.SeverityWarning {
			writer = deps.Stdout
		}
		fmt.Fprintln(writer, encoded(finding.Error()))
	}
	return report.HasErrors()
}

func printBackupSummary(summary backup.Summary, deps Dependencies) {
	succeeded, failed := 0, 0
	for _, job := range summary.Results {
		if job.Err != nil {
			failed++
			continue
		}
		succeeded++
	}
	fmt.Fprintf(deps.Stdout, "backup summary: %d succeeded, %d failed\n", succeeded, failed)
}
