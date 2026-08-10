package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

	cronpkg "github.com/ezgamehost/ezdbbackup/internal/cron"
	"github.com/ezgamehost/ezdbbackup/internal/terminal"
	"github.com/ezgamehost/ezdbbackup/internal/validation"
)

func runCron(ctx context.Context, args []string, deps Dependencies) int {
	if len(args) == 0 {
		printCronUsage(deps)
		return 2
	}
	switch args[0] {
	case "install":
		return runCronInstall(ctx, args[1:], deps)
	case "show":
		if len(args) != 1 {
			fmt.Fprintln(deps.Stderr, "cron show does not accept arguments")
			return 2
		}
		content, err := deps.Cron.Show()
		if err != nil {
			fmt.Fprintf(deps.Stderr, "cron show: %s\n", encoded(err))
			return 3
		}
		_, _ = io.WriteString(deps.Stdout, terminal.EncodeLines(content))
		return 0
	case "remove":
		if len(args) != 1 {
			fmt.Fprintln(deps.Stderr, "cron remove does not accept arguments")
			return 2
		}
		if err := deps.Cron.Remove(); err != nil {
			fmt.Fprintf(deps.Stderr, "cron remove: %s\n", encoded(err))
			return 3
		}
		fmt.Fprintln(deps.Stdout, "cron schedule removed")
		return 0
	default:
		fmt.Fprintf(deps.Stderr, "unknown cron command %s\n", encoded(args[0]))
		printCronUsage(deps)
		return 2
	}
}

func runCronInstall(ctx context.Context, args []string, deps Dependencies) int {
	flags := flag.NewFlagSet("cron install", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", defaultConfigPath, "configuration file")
	flags.Usage = func() {
		fmt.Fprintln(deps.Stderr, "usage: ezdbbackup cron install [--config <path>]")
	}
	if err := parseInterspersed(flags, args, map[string]bool{"config": true}); err != nil {
		fmt.Fprintln(deps.Stderr, encoded(err))
		flags.Usage()
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(deps.Stderr, "cron install does not accept positional arguments")
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
	binaryPath, err := executablePath(deps)
	if err != nil {
		fmt.Fprintf(deps.Stderr, "cron install validation: %s\n", encoded(err))
		return 2
	}
	report := deps.Validator.Check(ctx, cfg, nil, validation.Options{
		BinaryPath: binaryPath,
		ConfigPath: effectiveConfig,
	})
	if printValidationReport(report, deps) {
		return 2
	}
	content, err := cronpkg.Render(cfg, binaryPath, effectiveConfig)
	if err != nil {
		fmt.Fprintf(deps.Stderr, "cron install: render schedule: %s\n", encoded(err))
		return 2
	}
	if err := deps.Cron.Install(content); err != nil {
		fmt.Fprintf(deps.Stderr, "cron install: %s\n", encoded(err))
		return 3
	}
	fmt.Fprintln(deps.Stdout, "cron schedule installed")
	return 0
}

func printCronUsage(deps Dependencies) {
	fmt.Fprintln(deps.Stderr, "usage: ezdbbackup cron <install|show|remove>")
}
