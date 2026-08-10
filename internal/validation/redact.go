package validation

import (
	"sort"
	"strings"
)

type redactedCause struct {
	cause error
	text  string
}

func (e *redactedCause) Error() string { return e.text }

func (e *redactedCause) Unwrap() error { return e.cause }

func redactCause(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	ordered := appendNonEmpty(nil, secrets...)
	sort.SliceStable(ordered, func(left, right int) bool {
		return len(ordered[left]) > len(ordered[right])
	})
	text := err.Error()
	for _, secret := range ordered {
		text = strings.ReplaceAll(text, secret, "[REDACTED]")
	}
	return &redactedCause{cause: err, text: text}
}

func appendNonEmpty(values []string, additions ...string) []string {
	for _, addition := range additions {
		if addition != "" {
			values = append(values, addition)
		}
	}
	return values
}
