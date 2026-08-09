// Package jobresolve maps configured jobs into execution-time dependency options.
package jobresolve

import (
	"fmt"
	"os"

	"github.com/ezgamehost/ezdbbackup/internal/config"
	"github.com/ezgamehost/ezdbbackup/internal/dump"
	"github.com/ezgamehost/ezdbbackup/internal/storage"
)

// Resolver reads file-backed secrets while resolving a job for execution.
type Resolver struct {
	ReadFile func(string) ([]byte, error)
}

// OptionsResolver resolves dump and storage options from a configured job.
type OptionsResolver interface {
	Dump(config.JobConfig) (dump.Request, error)
	Storage(config.JobConfig) (storage.Options, error)
}

// Dump maps a job to a mysqldump request and resolves its password now.
func (r Resolver) Dump(job config.JobConfig) (dump.Request, error) {
	password, err := job.MySQL.PasswordRef().Resolve(r.readFile())
	if err != nil {
		return dump.Request{}, fmt.Errorf("resolve MySQL password: %w", err)
	}
	return dump.Request{
		Binary:       job.DumpBinary,
		Host:         job.MySQL.Host,
		Port:         job.MySQL.Port,
		User:         job.MySQL.User,
		Password:     password,
		AllDatabases: job.MySQL.Databases.All,
		Databases:    append([]string(nil), job.MySQL.Databases.Names...),
		ExtraArgs:    append([]string(nil), job.MySQL.ExtraArgs...),
	}, nil
}

// Storage maps a job to S3 client options and resolves explicit credentials now.
func (r Resolver) Storage(job config.JobConfig) (storage.Options, error) {
	options := storage.Options{
		Region:         job.S3.Region,
		Endpoint:       job.S3.Endpoint,
		ForcePathStyle: job.S3.ForcePathStyle,
	}
	if job.S3.AccessKeyID == "" && job.S3.AccessKeyIDFile == "" {
		return options, nil
	}

	readFile := r.readFile()
	accessKeyID, err := job.S3.AccessKeyIDRef().Resolve(readFile)
	if err != nil {
		return storage.Options{}, fmt.Errorf("resolve S3 access key ID: %w", err)
	}
	secretAccessKey, err := job.S3.SecretAccessKeyRef().Resolve(readFile)
	if err != nil {
		return storage.Options{}, fmt.Errorf("resolve S3 secret access key: %w", err)
	}
	sessionToken, err := job.S3.SessionTokenRef().Resolve(readFile)
	if err != nil {
		return storage.Options{}, fmt.Errorf("resolve S3 session token: %w", err)
	}
	options.Credentials = storage.Credentials{
		AccessKeyID:     accessKeyID,
		SecretAccessKey: secretAccessKey,
		SessionToken:    sessionToken,
		Explicit:        true,
	}
	return options, nil
}

func (r Resolver) readFile() func(string) ([]byte, error) {
	if r.ReadFile != nil {
		return r.ReadFile
	}
	return os.ReadFile
}
