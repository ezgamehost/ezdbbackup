package cli

import (
	"os"
	"time"

	"github.com/ezgamehost/ezdbbackup/internal/backup"
	"github.com/ezgamehost/ezdbbackup/internal/buildinfo"
	"github.com/ezgamehost/ezdbbackup/internal/config"
	"github.com/ezgamehost/ezdbbackup/internal/cron"
	"github.com/ezgamehost/ezdbbackup/internal/dump"
	"github.com/ezgamehost/ezdbbackup/internal/jobresolve"
	"github.com/ezgamehost/ezdbbackup/internal/logging"
	"github.com/ezgamehost/ezdbbackup/internal/stage"
	"github.com/ezgamehost/ezdbbackup/internal/storage"
	"github.com/ezgamehost/ezdbbackup/internal/validation"
)

// DefaultDependencies composes the production implementation used by main.
func DefaultDependencies() Dependencies {
	resolver := &jobresolve.Resolver{}
	dumpRunner := dump.ExecRunner{}
	stores := storage.AWSFactory{}
	validator := validation.Validator{
		Environment: validation.OSEnvironment{},
		Resolve:     resolver,
		Dump:        dumpRunner,
		Stores:      stores,
	}
	return Dependencies{
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
		Version:    buildinfo.String(),
		LoadConfig: config.Load,
		NewLogger: func(options logging.Options) (logging.Sink, error) {
			return logging.New(options)
		},
		NewBackup: func(sink logging.Sink) *backup.Service {
			return &backup.Service{
				Resolve: resolver,
				Dump:    dumpRunner,
				Stager:  stage.GzipStager{},
				Stores:  stores,
				Log:     sink,
			}
		},
		Validator:      validator,
		Cron:           cron.Manager{Path: cron.DefaultPath},
		ExecutablePath: os.Executable,
	}
}

func logOptions(configured config.LoggingConfig, invocationDebug bool) logging.Options {
	return logging.Options{
		Directory: configured.Directory,
		Debug:     configured.Debug || invocationDebug,
		Rotation: logging.RotationOptions{
			MaxSizeBytes: int64(configured.Rotation.MaxSizeMB) * 1024 * 1024,
			MaxFiles:     configured.Rotation.MaxFiles,
			MaxAge:       time.Duration(configured.Rotation.MaxAgeDays) * 24 * time.Hour,
			Compress:     configured.Rotation.Compress,
		},
	}
}
