// Package dump executes mysqldump without placing database credentials in its
// command-line arguments.
package dump

import (
	"strconv"

	"github.com/ezgamehost/ezdbbackup/internal/mysqldumpopt"
)

// Request describes a validated mysqldump invocation.
type Request struct {
	Binary       string
	RunAs        string
	Host         string
	Port         int
	User         string
	Password     string
	AllDatabases bool
	Databases    []string
	ExtraArgs    []string
}

// Args returns the deterministic mysqldump arguments for a backup request.
func Args(req Request) []string {
	args := connectionArgs(req)
	args = append(args, req.ExtraArgs...)
	return appendDatabaseScope(args, req)
}

func connectionArgs(req Request) []string {
	return []string{
		"--host=" + req.Host,
		"--port=" + strconv.Itoa(req.Port),
		"--user=" + req.User,
	}
}

func appendDatabaseScope(args []string, req Request) []string {
	if req.AllDatabases {
		return append(args, "--all-databases")
	}
	args = append(args, "--databases", "--")
	return append(args, req.Databases...)
}

// ProbeArgs returns the mysqldump arguments used to verify connectivity and
// permissions without emitting data or DDL.
func ProbeArgs(req Request) []string {
	// Option files may contain dump behavior such as result-file, init-command,
	// or replica controls, and some of those act while options are parsed. Keep
	// the probe's policy enforceable by disabling ordinary option files before
	// supplying owned connection fields. MySQL login-path files, where supported,
	// are restricted by MySQL to connection and authentication settings.
	args := []string{"--no-defaults"}
	args = append(args, connectionArgs(req)...)
	for _, arg := range req.ExtraArgs {
		if probeConnectionOption(arg) {
			args = append(args, arg)
		}
	}
	args = append(args,
		"--no-data",
		"--no-create-info",
		"--skip-triggers",
		"--skip-lock-tables",
		// The legacy spelling is supported by older MySQL and MariaDB clients;
		// current MySQL maps it and delete-source-logs to the same boolean.
		"--skip-delete-master-logs",
		"--skip-flush-logs",
	)
	return appendDatabaseScope(args, req)
}

// probeConnectionOptions is deliberately an allowlist: a backup may use many
// dump-shaping options, but a probe only needs transport and TLS settings.
var probeConnectionOptions = map[string]struct{}{
	"bind-address":            {},
	"compress":                {},
	"compression-algorithms":  {},
	"connect-timeout":         {},
	"default-auth":            {},
	"disable-ssl":             {},
	"enable-cleartext-plugin": {},
	"enable-ssl":              {},
	"get-server-public-key":   {},
	"max-allowed-packet":      {},
	"plugin-dir":              {},
	"protocol":                {},
	"server-public-key-path":  {},
	"skip-ssl":                {},
	"socket":                  {},
	"ssl":                     {},
	"ssl-ca":                  {},
	"ssl-capath":              {},
	"ssl-cert":                {},
	"ssl-cipher":              {},
	"ssl-crl":                 {},
	"ssl-crlpath":             {},
	"ssl-fips-mode":           {},
	"ssl-key":                 {},
	"ssl-mode":                {},
	"ssl-session-data":        {},
	"ssl-session-data-continue-on-failed-reuse": {},
	"ssl-verify-server-cert":                    {},
	"tls-ciphersuites":                          {},
	"tls-sni-servername":                        {},
	"tls-version":                               {},
	"zstd-compression-level":                    {},
}

func probeConnectionOption(arg string) bool {
	name, ok := mysqldumpopt.LongName(arg)
	if !ok {
		return false
	}
	_, ok = probeConnectionOptions[name]
	return ok
}
