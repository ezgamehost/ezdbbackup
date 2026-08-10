package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/ezgamehost/ezdbbackup/internal/validation"
)

func runValidate(ctx context.Context, args []string, deps Dependencies) int {
	flags := flag.NewFlagSet("validate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", defaultConfigPath, "configuration file")
	connectivity := flags.Bool("connectivity", false, "check MySQL and S3 connectivity")
	all := flags.Bool("all", false, "validate every configured job")
	_ = flags.Bool("debug", false, "accepted for CLI consistency; validation does not initialize logging")
	flags.Usage = func() {
		fmt.Fprintln(deps.Stderr, "usage: ezdbbackup validate [<job> | --all] [--connectivity] [--config <path>] [--debug]")
	}
	if err := parseInterspersed(flags, args, map[string]bool{"config": true}); err != nil {
		fmt.Fprintln(deps.Stderr, encoded(err))
		flags.Usage()
		return 2
	}
	positional := flags.Args()
	if len(positional) > 1 || (*all && len(positional) != 0) {
		fmt.Fprintln(deps.Stderr, "validate accepts one job or --all, but not both")
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
		fmt.Fprintf(deps.Stderr, "validation: %s\n", encoded(err))
		return 2
	}
	var selected []string
	if len(positional) == 1 {
		selected = []string{positional[0]}
	}
	report := deps.Validator.Check(ctx, cfg, selected, validation.Options{
		Connectivity: *connectivity,
		BinaryPath:   binaryPath,
		ConfigPath:   effectiveConfig,
	})
	if printValidationReport(report, deps) {
		return 2
	}
	fmt.Fprintln(deps.Stdout, "validation succeeded")
	return 0
}
