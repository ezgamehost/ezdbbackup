// Package cli parses ezdbbackup commands and maps outcomes to stable exit codes.
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/ezgamehost/ezdbbackup/internal/backup"
	"github.com/ezgamehost/ezdbbackup/internal/config"
	"github.com/ezgamehost/ezdbbackup/internal/logging"
	"github.com/ezgamehost/ezdbbackup/internal/validation"
)

const defaultConfigPath = "/etc/ezdbbackup/config.yml"

// Dependencies contains the process boundaries used by the CLI.
type Dependencies struct {
	Stdout         io.Writer
	Stderr         io.Writer
	Version        string
	LoadConfig     func(string) (*config.Config, config.Findings)
	NewLogger      func(logging.Options) (logging.Sink, error)
	NewBackup      func(logging.Sink) *backup.Service
	Validator      validation.Checker
	Cron           CronService
	ExecutablePath func() (string, error)
}

// CronService owns the managed system cron file.
type CronService interface {
	Install([]byte) error
	Show() ([]byte, error)
	Remove() error
}

// Run executes one CLI invocation without terminating the process.
func Run(ctx context.Context, args []string, deps Dependencies) int {
	if len(args) == 0 {
		printUsage(deps.Stderr)
		return 2
	}

	switch args[0] {
	case "version":
		if len(args) != 1 {
			fmt.Fprintln(deps.Stderr, "version does not accept arguments")
			return 2
		}
		fmt.Fprintf(deps.Stdout, "ezdbbackup %s\n", deps.Version)
		return 0
	case "backup":
		return runBackup(ctx, args[1:], deps)
	case "validate":
		return runValidate(ctx, args[1:], deps)
	case "cron":
		return runCron(ctx, args[1:], deps)
	default:
		fmt.Fprintf(deps.Stderr, "unknown command %q\n", args[0])
		printUsage(deps.Stderr)
		return 2
	}
}

func printUsage(writer io.Writer) {
	fmt.Fprintln(writer, "Usage: ezdbbackup <backup|validate|cron|version> [options]")
}

func parseInterspersed(flags *flag.FlagSet, args []string, valueFlags map[string]bool) error {
	ordered := make([]string, 0, len(args))
	positionals := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			positionals = append(positionals, args[index+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positionals = append(positionals, arg)
			continue
		}
		ordered = append(ordered, arg)
		name := strings.TrimLeft(arg, "-")
		if equals := strings.IndexByte(name, '='); equals >= 0 {
			name = name[:equals]
		}
		if valueFlags[name] && !strings.ContainsRune(arg, '=') && index+1 < len(args) {
			next := args[index+1]
			if strings.HasPrefix(next, "-") {
				return flags.Parse([]string{arg})
			}
			index++
			ordered = append(ordered, next)
		}
	}
	ordered = append(ordered, positionals...)
	return flags.Parse(ordered)
}

func effectiveAbsolutePath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve absolute path for %q: %w", path, err)
	}
	return filepath.Clean(absolute), nil
}
