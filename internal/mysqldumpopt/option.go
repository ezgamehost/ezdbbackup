// Package mysqldumpopt parses the long-option spelling accepted at the
// ezdbbackup/mysqldump boundary.
package mysqldumpopt

import "strings"

// LongName returns a canonical long-option name. Option names are compared
// case-insensitively and with underscores treated as hyphens, matching MySQL's
// option spelling aliases. Values following the first '=' are not returned.
func LongName(arg string) (string, bool) {
	if !strings.HasPrefix(arg, "--") || arg == "--" {
		return "", false
	}
	raw := strings.TrimPrefix(arg, "--")
	if before, _, found := strings.Cut(raw, "="); found {
		raw = before
	}
	if raw == "" {
		return "", false
	}
	for i := 0; i < len(raw); i++ {
		value := raw[i]
		if value != '-' && value != '_' &&
			(value < 'a' || value > 'z') &&
			(value < 'A' || value > 'Z') &&
			(value < '0' || value > '9') {
			return "", false
		}
	}
	if raw[0] == '-' || raw[0] == '_' || raw[len(raw)-1] == '-' || raw[len(raw)-1] == '_' {
		return "", false
	}
	return strings.ToLower(strings.ReplaceAll(raw, "_", "-")), true
}
