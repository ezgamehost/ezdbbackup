// Package validation checks configuration, local execution prerequisites, and
// optional remote connectivity without changing the system.
package validation

import "fmt"

// Severity classifies a validation finding.
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Finding describes one failed or cautionary validation check.
type Finding struct {
	Severity Severity
	Job      string
	Check    string
	Message  string
	Cause    error
}

func (f Finding) Error() string {
	prefix := string(f.Severity)
	if f.Job != "" {
		prefix += " job " + f.Job
	}
	if f.Check != "" {
		prefix += " " + f.Check
	}
	if f.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", prefix, f.Message, f.Cause)
	}
	return fmt.Sprintf("%s: %s", prefix, f.Message)
}

func (f Finding) Unwrap() error { return f.Cause }

// Report aggregates validation findings. Append returns a new report so
// callers can combine reports without mutating either input.
type Report struct {
	Findings []Finding
}

func (r Report) Append(findings ...Finding) Report {
	combined := make([]Finding, 0, len(r.Findings)+len(findings))
	combined = append(combined, r.Findings...)
	combined = append(combined, findings...)
	return Report{Findings: combined}
}

func (r Report) HasErrors() bool {
	for _, finding := range r.Findings {
		if finding.Severity == SeverityError {
			return true
		}
	}
	return false
}
