package backup

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/ezgamehost/ezdbbackup/internal/config"
)

// JobResult is one selected job's result and any execution error.
type JobResult struct {
	Result
	Err error
}

// Summary contains results in execution order.
type Summary struct {
	Results []JobResult
}

// JobNames returns selected job names in result order.
func (s Summary) JobNames() []string {
	names := make([]string, len(s.Results))
	for i, result := range s.Results {
		names[i] = result.Job
	}
	return names
}

// HasFailures reports whether any selected job failed or was rejected.
func (s Summary) HasFailures() bool {
	for _, result := range s.Results {
		if result.Err != nil {
			return true
		}
	}
	return false
}

// RunMany validates the complete explicit selection, then runs jobs
// sequentially in lexical name order. With no names it selects all enabled jobs.
func (s *Service) RunMany(ctx context.Context, cfg *config.Config, names []string) Summary {
	if cfg == nil {
		return Summary{Results: []JobResult{{Err: stageError("configuration", errors.New("configuration is required"))}}}
	}

	selected := append([]string(nil), names...)
	explicit := len(selected) > 0
	if !explicit {
		selected = cfg.EnabledJobNames()
	} else {
		sort.Strings(selected)
	}

	if explicit {
		selectionErrors := make(map[string]error)
		for _, name := range selected {
			job, exists := cfg.Jobs[name]
			switch {
			case !exists:
				selectionErrors[name] = fmt.Errorf("job %q not found", name)
			case !job.Enabled:
				selectionErrors[name] = fmt.Errorf("job %q is disabled", name)
			}
		}
		if len(selectionErrors) > 0 {
			results := make([]JobResult, 0, len(selected))
			for _, name := range selected {
				selectionErr := selectionErrors[name]
				if selectionErr == nil {
					selectionErr = errors.New("job not run because explicit job selection was rejected")
				}
				results = append(results, JobResult{
					Result: Result{Job: name},
					Err:    stageError("configuration", selectionErr),
				})
			}
			return Summary{Results: results}
		}
	}

	results := make([]JobResult, 0, len(selected))
	for _, name := range selected {
		result, err := s.Run(ctx, name, cfg.Jobs[name])
		if result.Job == "" {
			result.Job = name
		}
		results = append(results, JobResult{Result: result, Err: err})
	}
	return Summary{Results: results}
}
