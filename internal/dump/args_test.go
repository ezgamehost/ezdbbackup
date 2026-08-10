package dump

import (
	"slices"
	"testing"
)

// This fails if host, port, user, extra arguments, or selected database names
// are omitted or reordered in the mysqldump command line.
func TestArgsForSelectedDatabases(t *testing.T) {
	req := Request{
		Host: "db.internal", Port: 3307, User: "backup",
		Databases: []string{"app", "analytics"},
		ExtraArgs: []string{"--single-transaction"},
	}
	want := []string{
		"--no-defaults", "--host=db.internal", "--port=3307", "--user=backup",
		"--single-transaction", "--databases", "--", "app", "analytics",
	}
	if got := Args(req); !slices.Equal(got, want) {
		t.Fatalf("Args() = %#v, want %#v", got, want)
	}
}

// This fails if an all-database backup is expressed as a selected-database
// backup, which would change the data mysqldump exports.
func TestArgsForAllDatabases(t *testing.T) {
	req := Request{Host: "db.internal", Port: 3307, User: "backup", AllDatabases: true}
	want := []string{"--no-defaults", "--host=db.internal", "--port=3307", "--user=backup", "--all-databases"}
	if got := Args(req); !slices.Equal(got, want) {
		t.Fatalf("Args() = %#v, want %#v", got, want)
	}
}

// This fails if backup and connectivity validation stop sharing the same
// safety-first option-file and configured connection baseline.
func TestBackupAndProbeArgsShareNoDefaultsConnectionBaseline(t *testing.T) {
	req := Request{Host: "db.internal", Port: 3307, User: "backup", Databases: []string{"app"}}
	want := []string{"--no-defaults", "--host=db.internal", "--port=3307", "--user=backup"}
	for name, args := range map[string][]string{"backup": Args(req), "probe": ProbeArgs(req)} {
		if len(args) < len(want) || !slices.Equal(args[:len(want)], want) {
			t.Fatalf("%s connection baseline = %#v, want %#v", name, args, want)
		}
	}
}

// This fails if probing writes data or DDL, executes triggers, or requests a
// result file instead of using the process output stream.
func TestProbeArgs(t *testing.T) {
	req := Request{
		Host: "db.internal", Port: 3307, User: "backup", Databases: []string{"app"},
		ExtraArgs: []string{
			"--single-transaction",
			"--quick",
			"--delete-source-logs",
			"--delete-master-logs",
			"--flush-logs",
			"--ssl-mode=VERIFY_IDENTITY",
			"--SSL_CA=/etc/mysql/ca.pem",
			"--tls_version=TLSv1.3",
		},
	}
	want := []string{
		"--no-defaults",
		"--host=db.internal", "--port=3307", "--user=backup",
		"--ssl-mode=VERIFY_IDENTITY", "--SSL_CA=/etc/mysql/ca.pem", "--tls_version=TLSv1.3",
		"--no-data", "--no-create-info", "--skip-triggers",
		"--skip-lock-tables", "--skip-delete-master-logs", "--skip-flush-logs",
		"--databases", "--", "app",
	}
	if got := ProbeArgs(req); !slices.Equal(got, want) {
		t.Fatalf("ProbeArgs() = %#v, want %#v", got, want)
	}
}

// This fails if a connection/authentication/TLS option needed by the real
// backup is dropped from the otherwise restrictive probe allowlist.
func TestProbeArgsPreservesAuditedConnectionOptions(t *testing.T) {
	options := []string{
		"--protocol=tcp",
		"--socket=/run/mysqld/mysqld.sock",
		"--connect-timeout=10",
		"--ssl-mode=VERIFY_IDENTITY",
		"--ssl-ca=/etc/mysql/ca.pem",
		"--ssl-cert=/etc/mysql/client.pem",
		"--ssl-key=/etc/mysql/client-key.pem",
		"--ssl-crl=/etc/mysql/crl.pem",
		"--ssl-session-data=/run/mysql-session.pem",
		"--ssl-verify-server-cert",
		"--tls-version=TLSv1.3",
		"--tls-ciphersuites=TLS_AES_256_GCM_SHA384",
		"--default-auth=caching_sha2_password",
		"--get-server-public-key",
		"--server-public-key-path=/etc/mysql/server-key.pem",
	}
	for _, option := range options {
		t.Run(option, func(t *testing.T) {
			got := ProbeArgs(Request{ExtraArgs: []string{option}, Databases: []string{"app"}})
			if !slices.Contains(got, option) {
				t.Fatalf("ProbeArgs() = %#v, want audited connection option %q", got, option)
			}
		})
	}
}
