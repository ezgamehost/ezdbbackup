package jobresolve

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ezgamehost/ezdbbackup/internal/config"
	"github.com/ezgamehost/ezdbbackup/internal/dump"
	"github.com/ezgamehost/ezdbbackup/internal/storage"
)

// A path substitution after the secure open must not change the bytes read,
// and the pinned descriptor must be closed on return.
func TestResolverReadsFileSecretThroughOnePinnedDescriptor(t *testing.T) {
	directory := secureResolverTestDir(t)
	trusted := filepath.Join(directory, "trusted-secret")
	attacker := filepath.Join(directory, "attacker-secret")
	if err := os.WriteFile(trusted, []byte("trusted-value\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(attacker, []byte("attacker-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "mysql-secret")
	if err := os.Symlink(trusted, link); err != nil {
		t.Fatal(err)
	}
	var pinned *os.File
	resolver := Resolver{afterSecretOpen: func(file *os.File) {
		pinned = file
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(attacker, link); err != nil {
			t.Fatal(err)
		}
	}}
	request, err := resolver.Dump(config.JobConfig{
		RunAs: currentResolverUsername(t),
		MySQL: config.MySQLConfig{PasswordFile: link},
	})
	if err != nil {
		t.Fatalf("Dump() error = %v", err)
	}
	if request.Password != "trusted-value" {
		t.Fatalf("resolved password = %q, want trusted descriptor bytes", request.Password)
	}
	if pinned == nil {
		t.Fatal("secret-open hook did not receive descriptor")
	}
	if _, err := pinned.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("secret descriptor Stat() error = %v, want closed", err)
	}
}

// Replacing a previously checked path with an unsafe file must be rejected at
// resolution time instead of relying on the earlier validation result.
func TestResolverRejectsUnsafeOrOversizedFileSecretAtPointOfUse(t *testing.T) {
	for _, tt := range []struct {
		name     string
		contents string
		mode     os.FileMode
		want     string
	}{
		{name: "group writable substitute", contents: "attacker", mode: 0o660, want: "writable"},
		{name: "oversized substitute", contents: strings.Repeat("x", config.MaxSecretFileBytes+1), mode: 0o600, want: "too large"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(secureResolverTestDir(t), "secret")
			if err := os.WriteFile(path, []byte(tt.contents), tt.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tt.mode); err != nil {
				t.Fatal(err)
			}
			_, err := (Resolver{}).Dump(config.JobConfig{
				RunAs: currentResolverUsername(t),
				MySQL: config.MySQLConfig{PasswordFile: path},
			})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Dump() error = %v, want %q rejection", err, tt.want)
			}
		})
	}
}

func secureResolverTestDir(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return directory
}

func currentResolverUsername(t *testing.T) string {
	t.Helper()
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	return account.Username
}

func TestResolverDumpMapsJobAndResolvesPasswordAtExecutionTime(t *testing.T) {
	password := "first-password\n"
	resolver := Resolver{readFile: func(path string) ([]byte, error) {
		if path != "/secrets/mysql" {
			t.Fatalf("ReadFile() path = %q, want /secrets/mysql", path)
		}
		return []byte(password), nil
	}}
	job := config.JobConfig{
		DumpBinary: "/opt/mysql/bin/mysqldump",
		MySQL: config.MySQLConfig{
			Host:         "db.internal",
			Port:         4406,
			User:         "backup-user",
			PasswordFile: "/secrets/mysql",
			Databases:    config.DatabaseSelection{Names: []string{"app", "analytics"}},
			ExtraArgs:    []string{"--single-transaction", "--quick"},
		},
	}

	got, err := resolver.Dump(job)
	if err != nil {
		t.Fatalf("Dump() error = %v", err)
	}
	want := dump.Request{
		Binary:       "/opt/mysql/bin/mysqldump",
		Host:         "db.internal",
		Port:         4406,
		User:         "backup-user",
		Password:     "first-password",
		AllDatabases: false,
		Databases:    []string{"app", "analytics"},
		ExtraArgs:    []string{"--single-transaction", "--quick"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Dump() = %#v, want %#v", got, want)
	}

	password = "rotated-password\n"
	got, err = resolver.Dump(job)
	if err != nil {
		t.Fatalf("Dump() after secret rotation error = %v", err)
	}
	if got.Password != "rotated-password" {
		t.Fatalf("Dump() password after secret rotation = %q, want rotated-password", got.Password)
	}
}

func TestResolverDumpMapsAllDatabaseSelection(t *testing.T) {
	got, err := (Resolver{}).Dump(config.JobConfig{
		MySQL: config.MySQLConfig{Databases: config.DatabaseSelection{All: true}},
	})
	if err != nil {
		t.Fatalf("Dump() error = %v", err)
	}
	if !got.AllDatabases || len(got.Databases) != 0 {
		t.Fatalf("Dump() database selection = all:%t names:%v, want all databases", got.AllDatabases, got.Databases)
	}
}

func TestResolverStorageMapsJobAndResolvesCredentialFilesAtExecutionTime(t *testing.T) {
	values := map[string]string{
		"/secrets/access":  "access-one\n",
		"/secrets/secret":  "secret-one\n",
		"/secrets/session": "session-one\n",
	}
	resolver := Resolver{readFile: func(path string) ([]byte, error) {
		value, ok := values[path]
		if !ok {
			return nil, errors.New("unexpected secret path")
		}
		return []byte(value), nil
	}}
	job := config.JobConfig{S3: config.S3Config{
		Region:              "eu-west-2",
		Endpoint:            "https://objects.internal",
		ForcePathStyle:      true,
		AccessKeyIDFile:     "/secrets/access",
		SecretAccessKeyFile: "/secrets/secret",
		SessionTokenFile:    "/secrets/session",
	}}

	got, err := resolver.Storage(job)
	if err != nil {
		t.Fatalf("Storage() error = %v", err)
	}
	want := storage.Options{
		Region:         "eu-west-2",
		Endpoint:       "https://objects.internal",
		ForcePathStyle: true,
		Credentials: storage.Credentials{
			AccessKeyID:     "access-one",
			SecretAccessKey: "secret-one",
			SessionToken:    "session-one",
			Explicit:        true,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Storage() = %#v, want %#v", got, want)
	}

	values["/secrets/access"] = "access-two\n"
	values["/secrets/secret"] = "secret-two\n"
	values["/secrets/session"] = "session-two\n"
	got, err = resolver.Storage(job)
	if err != nil {
		t.Fatalf("Storage() after secret rotation error = %v", err)
	}
	if got.Credentials.AccessKeyID != "access-two" || got.Credentials.SecretAccessKey != "secret-two" || got.Credentials.SessionToken != "session-two" {
		t.Fatalf("Storage() credentials after secret rotation = %#v", got.Credentials)
	}
}

func TestResolverStorageLeavesDefaultCredentialChainEnabled(t *testing.T) {
	reads := 0
	resolver := Resolver{readFile: func(string) ([]byte, error) {
		reads++
		return nil, errors.New("must not read")
	}}

	got, err := resolver.Storage(config.JobConfig{S3: config.S3Config{Region: "us-east-1"}})
	if err != nil {
		t.Fatalf("Storage() error = %v", err)
	}
	if got.Credentials != (storage.Credentials{}) {
		t.Fatalf("Storage() credentials = %#v, want zero value for default credential chain", got.Credentials)
	}
	if reads != 0 {
		t.Fatalf("Storage() secret reads = %d, want 0", reads)
	}
}

func TestResolverReportsSecretReadFailureWithoutSecretValues(t *testing.T) {
	wantErr := errors.New("read denied")
	resolver := Resolver{readFile: func(string) ([]byte, error) { return nil, wantErr }}

	_, err := resolver.Dump(config.JobConfig{MySQL: config.MySQLConfig{PasswordFile: "/secrets/mysql"}})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Dump() error = %v, want wrapped read error", err)
	}
}
