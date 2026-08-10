package backup

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/ezgamehost/ezdbbackup/internal/config"
	"github.com/ezgamehost/ezdbbackup/internal/dump"
	"github.com/ezgamehost/ezdbbackup/internal/jobresolve"
	"github.com/ezgamehost/ezdbbackup/internal/stage"
	"github.com/ezgamehost/ezdbbackup/internal/storage"
)

func TestRunManyIsLexicalAndContinuesAfterFailure(t *testing.T) {
	var order []string
	middleErr := errors.New("middle dump failed")
	service := Service{
		Resolve: jobresolve.Resolver{},
		Dump: dumpFunc(func(_ context.Context, request dump.Request, writer io.Writer) error {
			order = append(order, request.User)
			if request.User == "middle" {
				return middleErr
			}
			_, err := io.WriteString(writer, "SELECT 1;\n")
			return err
		}),
		Stager: stage.GzipStager{},
		Stores: &recordingFactory{store: &uploadStore{}},
	}
	cfg := configWithJobs(t, map[string]bool{"zeta": true, "alpha": true, "middle": true})

	summary := service.RunMany(context.Background(), cfg, nil)
	if got := summary.JobNames(); !slices.Equal(got, []string{"alpha", "middle", "zeta"}) {
		t.Fatalf("summary order = %v, want lexical order", got)
	}
	if !slices.Equal(order, []string{"alpha", "middle", "zeta"}) {
		t.Fatalf("dump order = %v, want lexical sequential execution", order)
	}
	if !summary.HasFailures() || len(summary.Results) != 3 {
		t.Fatalf("summary = %#v, want three results with a failure", summary)
	}
	if !errors.Is(summary.Results[1].Err, middleErr) {
		t.Fatalf("middle result error = %v, want middle dump failure", summary.Results[1].Err)
	}
	if summary.Results[0].Err != nil || summary.Results[2].Err != nil {
		t.Fatalf("successful job errors = alpha:%v zeta:%v", summary.Results[0].Err, summary.Results[2].Err)
	}
}

func TestRunManyWithNoNamesRunsOnlyEnabledJobs(t *testing.T) {
	var order []string
	service := summaryRecordingService(&order)
	cfg := configWithJobs(t, map[string]bool{"zeta": true, "disabled": false, "alpha": true})

	summary := service.RunMany(context.Background(), cfg, nil)
	if got := summary.JobNames(); !slices.Equal(got, []string{"alpha", "zeta"}) {
		t.Fatalf("summary jobs = %v, want enabled jobs only", got)
	}
	if !slices.Equal(order, []string{"alpha", "zeta"}) {
		t.Fatalf("dump order = %v, want enabled jobs only", order)
	}
}

func TestRunManySortsExplicitNames(t *testing.T) {
	var order []string
	service := summaryRecordingService(&order)
	cfg := configWithJobs(t, map[string]bool{"zeta": true, "alpha": true, "middle": true})

	summary := service.RunMany(context.Background(), cfg, []string{"zeta", "alpha"})
	if got := summary.JobNames(); !slices.Equal(got, []string{"alpha", "zeta"}) {
		t.Fatalf("summary jobs = %v, want sorted explicit jobs", got)
	}
	if !slices.Equal(order, []string{"alpha", "zeta"}) {
		t.Fatalf("dump order = %v, want sorted explicit jobs", order)
	}
}

func TestRunManyRejectsMissingAndDisabledExplicitJobsBeforeAnyDump(t *testing.T) {
	var order []string
	service := summaryRecordingService(&order)
	cfg := configWithJobs(t, map[string]bool{"alpha": true, "disabled": false})

	summary := service.RunMany(context.Background(), cfg, []string{"missing", "alpha", "disabled"})
	if len(order) != 0 {
		t.Fatalf("dump order = %v, want no dump before complete explicit-name validation", order)
	}
	if got := summary.JobNames(); !slices.Equal(got, []string{"alpha", "disabled", "missing"}) {
		t.Fatalf("summary jobs = %v, want every selected name in lexical order", got)
	}
	if !summary.HasFailures() || len(summary.Results) != 3 {
		t.Fatalf("summary = %#v, want rejected result for every selected name", summary)
	}
	if !strings.Contains(summary.Results[1].Err.Error(), "disabled") {
		t.Fatalf("disabled error = %v", summary.Results[1].Err)
	}
	if !strings.Contains(summary.Results[2].Err.Error(), "not found") {
		t.Fatalf("missing error = %v", summary.Results[2].Err)
	}
	if summary.Results[0].Err == nil || !strings.Contains(summary.Results[0].Err.Error(), "selection") {
		t.Fatalf("valid-but-not-run error = %v, want selection rejection", summary.Results[0].Err)
	}
}

func TestRunManyResultErrorsRedactResolvedSecretsAndPreserveCause(t *testing.T) {
	uploadErr := errors.New("upload rejected overlap and overlap-credential")
	service := Service{
		Resolve: jobresolve.Resolver{},
		Dump: dumpFunc(func(_ context.Context, _ dump.Request, writer io.Writer) error {
			_, err := io.WriteString(writer, "SELECT 1;\n")
			return err
		}),
		Stager: stage.GzipStager{},
		Stores: &recordingFactory{store: &uploadStore{uploadErr: uploadErr}},
	}
	cfg := configWithJobs(t, map[string]bool{"production": true})
	job := cfg.Jobs["production"]
	job.MySQL.Password = "mysql-sensitive"
	job.S3.AccessKeyID = "overlap"
	job.S3.SecretAccessKey = "overlap-credential"
	cfg.Jobs["production"] = job

	summary := service.RunMany(context.Background(), cfg, nil)
	if len(summary.Results) != 1 {
		t.Fatalf("summary results = %d, want 1", len(summary.Results))
	}
	err := summary.Results[0].Err
	if !errors.Is(err, uploadErr) {
		t.Fatalf("summary error = %v, want original upload cause", err)
	}
	if got := err.Error(); got != "backup stage s3_upload: upload rejected [REDACTED] and [REDACTED]" {
		t.Fatalf("summary error = %q, want complete redaction", got)
	}
	for _, secret := range []string{"mysql-sensitive", "overlap", "overlap-credential"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("summary error contains secret %q: %v", secret, err)
		}
	}
}

func summaryRecordingService(order *[]string) Service {
	return Service{
		Resolve: jobresolve.Resolver{},
		Dump: dumpFunc(func(_ context.Context, request dump.Request, writer io.Writer) error {
			*order = append(*order, request.User)
			_, err := io.WriteString(writer, "SELECT 1;\n")
			return err
		}),
		Stager: stage.GzipStager{},
		Stores: &recordingFactory{store: &uploadStore{}},
	}
}

func configWithJobs(t *testing.T, jobs map[string]bool) *config.Config {
	t.Helper()
	cfg := &config.Config{Jobs: make(map[string]config.JobConfig, len(jobs))}
	for name, enabled := range jobs {
		cfg.Jobs[name] = config.JobConfig{
			Enabled: enabled,
			TempDir: secureBackupTempDir(t),
			MySQL: config.MySQLConfig{
				User:      name,
				Databases: config.DatabaseSelection{All: true},
			},
			S3: config.S3Config{Bucket: "backups", Region: "us-east-1"},
		}
	}
	return cfg
}

var _ storage.Store = (*uploadStore)(nil)
