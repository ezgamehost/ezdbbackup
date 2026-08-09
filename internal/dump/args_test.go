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
		"--host=db.internal", "--port=3307", "--user=backup",
		"--single-transaction", "--databases", "app", "analytics",
	}
	if got := Args(req); !slices.Equal(got, want) {
		t.Fatalf("Args() = %#v, want %#v", got, want)
	}
}

// This fails if an all-database backup is expressed as a selected-database
// backup, which would change the data mysqldump exports.
func TestArgsForAllDatabases(t *testing.T) {
	req := Request{Host: "db.internal", Port: 3307, User: "backup", AllDatabases: true}
	want := []string{"--host=db.internal", "--port=3307", "--user=backup", "--all-databases"}
	if got := Args(req); !slices.Equal(got, want) {
		t.Fatalf("Args() = %#v, want %#v", got, want)
	}
}

// This fails if probing writes data or DDL, executes triggers, or requests a
// result file instead of using the process output stream.
func TestProbeArgs(t *testing.T) {
	req := Request{Host: "db.internal", Port: 3307, User: "backup", Databases: []string{"app"}}
	want := []string{
		"--host=db.internal", "--port=3307", "--user=backup", "--databases", "app",
		"--no-data", "--no-create-info", "--skip-triggers",
	}
	if got := ProbeArgs(req); !slices.Equal(got, want) {
		t.Fatalf("ProbeArgs() = %#v, want %#v", got, want)
	}
}
