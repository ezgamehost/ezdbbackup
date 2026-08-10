package cli

import (
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/ezgamehost/ezdbbackup/internal/config"
	"github.com/ezgamehost/ezdbbackup/internal/cron"
	"github.com/ezgamehost/ezdbbackup/internal/dump"
	"github.com/ezgamehost/ezdbbackup/internal/jobresolve"
	"github.com/ezgamehost/ezdbbackup/internal/logging"
	"github.com/ezgamehost/ezdbbackup/internal/stage"
	"github.com/ezgamehost/ezdbbackup/internal/storage"
	"github.com/ezgamehost/ezdbbackup/internal/validation"
)

func TestLogOptionsMapsConfiguredRotationAndInvocationDebug(t *testing.T) {
	configured := config.LoggingConfig{
		Directory: "/logs",
		Debug:     false,
		Rotation:  config.RotationConfig{MaxSizeMB: 17, MaxFiles: 4, MaxAgeDays: 12, Compress: true},
	}
	want := logging.Options{
		Directory: "/logs",
		Debug:     true,
		Rotation: logging.RotationOptions{
			MaxSizeBytes: 17 * 1024 * 1024,
			MaxFiles:     4,
			MaxAge:       12 * 24 * time.Hour,
			Compress:     true,
		},
	}
	got, err := logOptions(configured, true)
	if err != nil {
		t.Fatalf("logOptions() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("logOptions() = %#v, want %#v", got, want)
	}
	configured.Debug = true
	got, err = logOptions(configured, false)
	if err != nil {
		t.Fatalf("logOptions() configured debug error = %v", err)
	}
	if !got.Debug {
		t.Fatal("configured debug was disabled by invocation options")
	}
}

func TestLogOptionsRejectsOverflow(t *testing.T) {
	maxSizeMB := int(^uint64(0)>>1) / (1024 * 1024)
	maxAgeDays := int(^uint64(0)>>1) / int(24*time.Hour)
	for _, configured := range []config.LoggingConfig{
		{Rotation: config.RotationConfig{MaxSizeMB: maxSizeMB + 1, MaxAgeDays: 1}},
		{Rotation: config.RotationConfig{MaxSizeMB: 1, MaxAgeDays: maxAgeDays + 1}},
	} {
		if _, err := logOptions(configured, false); err == nil {
			t.Fatalf("logOptions(%+v) error = nil, want overflow", configured.Rotation)
		}
	}
}

func TestLogOptionsRejectsUnreasonablyLargeRotationHistory(t *testing.T) {
	_, err := logOptions(config.LoggingConfig{
		Directory: "/var/log/ezdbbackup",
		Rotation: config.RotationConfig{
			MaxSizeMB:  1,
			MaxFiles:   1001,
			MaxAgeDays: 1,
		},
	}, false)
	if err == nil {
		t.Fatal("logOptions() accepted max_files above the supported bound")
	}
}

func TestDefaultDependenciesComposeRealComponentsWithSharedResolver(t *testing.T) {
	deps := DefaultDependencies()
	if deps.Stdout != os.Stdout || deps.Stderr != os.Stderr || deps.Version == "" {
		t.Fatalf("process defaults = stdout:%v stderr:%v version:%q", deps.Stdout, deps.Stderr, deps.Version)
	}
	if deps.LoadConfig == nil || deps.NewLogger == nil || deps.NewBackup == nil || deps.ExecutablePath == nil {
		t.Fatal("default constructor functions must be configured")
	}
	validator, ok := deps.Validator.(validation.Validator)
	if !ok {
		t.Fatalf("validator type = %T", deps.Validator)
	}
	if _, ok := validator.Environment.(validation.OSEnvironment); !ok {
		t.Fatalf("environment type = %T", validator.Environment)
	}
	if _, ok := validator.Dump.(dump.ExecRunner); !ok {
		t.Fatalf("validator dump type = %T", validator.Dump)
	}
	if _, ok := validator.Stores.(storage.AWSFactory); !ok {
		t.Fatalf("validator storage type = %T", validator.Stores)
	}
	manager, ok := deps.Cron.(cron.Manager)
	if !ok || manager.Path != cron.DefaultPath {
		t.Fatalf("cron dependency = %#v", deps.Cron)
	}

	service := deps.NewBackup(discardSink{})
	if _, ok := service.Resolve.(*jobresolve.Resolver); !ok {
		t.Fatalf("backup resolver type = %T", service.Resolve)
	}
	if service.Resolve != validator.Resolve {
		t.Fatal("backup and validation do not share the same resolver")
	}
	if _, ok := service.Dump.(dump.ExecRunner); !ok {
		t.Fatalf("backup dump type = %T", service.Dump)
	}
	if _, ok := service.Stager.(stage.GzipStager); !ok {
		t.Fatalf("backup stager type = %T", service.Stager)
	}
	if _, ok := service.Stores.(storage.AWSFactory); !ok {
		t.Fatalf("backup storage type = %T", service.Stores)
	}
}
