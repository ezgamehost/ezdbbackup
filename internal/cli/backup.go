package cli

import (
	"context"
	"flag"
	"fmt"

	"github.com/ezgamehost/ezdbbackup/internal/backup"
	"github.com/ezgamehost/ezdbbackup/internal/config"
	"github.com/ezgamehost/ezdbbackup/internal/validation"
)

func runBackup(ctx context.Context, args []string, deps Dependencies) int {
	flags := flag.NewFlagSet("backup", flag.ContinueOnError)
	flags.SetOutput(deps.Stderr)
	configPath := flags.String("config", defaultConfigPath, "configuration file")
	debug := flags.Bool("debug", false, "enable debug logging")
	all := flags.Bool("all", false, "back up every enabled job")
	flags.Usage = func() {
		fmt.Fprintln(deps.Stderr, "usage: ezdbbackup backup <job> [--config <path>] [--debug]")
		fmt.Fprintln(deps.Stderr, "       ezdbbackup backup --all [--config <path>] [--debug]")
	}
	if err := parseInterspersed(flags, args, map[string]bool{"config": true}); err != nil {
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
		fmt.Fprintf(deps.Stderr, "configuration path: %v\n", err)
		return 2
	}
	cfg, findings := deps.LoadConfig(effectiveConfig)
	if printConfigFindings(findings, deps) {
		return 2
	}

	var selected []string
	if *all {
		selected = cfg.EnabledJobNames()
	} else {
		name := positional[0]
		job, exists := cfg.Jobs[name]
		if !exists {
			fmt.Fprintf(deps.Stderr, "backup: job %q is not configured\n", name)
			return 2
		}
		if !job.Enabled {
			fmt.Fprintf(deps.Stderr, "backup: job %q is disabled\n", name)
			return 2
		}
		selected = []string{name}
	}
	if *all && len(selected) == 0 {
		printBackupSummary(backup.Summary{}, deps)
		return 0
	}

	binaryPath, err := executablePath(deps)
	if err != nil {
		fmt.Fprintf(deps.Stderr, "backup validation: %v\n", err)
		return 2
	}
	report := deps.Validator.Check(ctx, cfg, selected, validation.Options{
		BinaryPath: binaryPath,
		ConfigPath: effectiveConfig,
	})
	if printValidationReport(report, deps) {
		return 2
	}

	options, err := logOptions(cfg.Logging, *debug)
	if err != nil {
		fmt.Fprintf(deps.Stderr, "backup: invalid logging configuration: %v\n", err)
		return 2
	}
	logger, err := deps.NewLogger(options)
	if err != nil {
		fmt.Fprintf(deps.Stderr, "backup: initialize logging: %v\n", err)
		return 1
	}
	service := deps.NewBackup(logger)
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

func executablePath(deps Dependencies) (string, error) {
	path, err := deps.ExecutablePath()
	if err != nil {
		return "", fmt.Errorf("resolve executable path: %w", err)
	}
	return effectiveAbsolutePath(path)
}

func printConfigFindings(findings config.Findings, deps Dependencies) bool {
	for _, finding := range findings {
		writer := deps.Stderr
		if finding.Warning {
			writer = deps.Stdout
		}
		fmt.Fprintln(writer, finding.String())
	}
	return findings.HasErrors()
}

func printValidationReport(report validation.Report, deps Dependencies) bool {
	for _, finding := range report.Findings {
		writer := deps.Stderr
		if finding.Severity == validation.SeverityWarning {
			writer = deps.Stdout
		}
		fmt.Fprintln(writer, finding.Error())
	}
	return report.HasErrors()
}

func printBackupSummary(summary backup.Summary, deps Dependencies) {
	succeeded, failed := 0, 0
	for _, job := range summary.Results {
		if job.Err != nil {
			failed++
			fmt.Fprintf(deps.Stdout, "%s: failed: %v\n", job.Job, job.Err)
			continue
		}
		succeeded++
		fmt.Fprintf(deps.Stdout, "%s: success: %s (%d bytes)\n", job.Job, job.ObjectKey, job.Size)
	}
	fmt.Fprintf(deps.Stdout, "backup summary: %d succeeded, %d failed\n", succeeded, failed)
}
