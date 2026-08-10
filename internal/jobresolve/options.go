// Package jobresolve maps configured jobs into execution-time dependency options.
package jobresolve

import (
	"bytes"
	"errors"
	"fmt"
	"os"

	"github.com/ezgamehost/ezdbbackup/internal/config"
	"github.com/ezgamehost/ezdbbackup/internal/dump"
	"github.com/ezgamehost/ezdbbackup/internal/securepath"
	"github.com/ezgamehost/ezdbbackup/internal/storage"
)

// Resolver reads file-backed secrets while resolving a job for execution.
type Resolver struct {
	readFile        func(string) ([]byte, error)
	afterSecretOpen func(*os.File)
}

// OptionsResolver resolves dump and storage options from a configured job.
type OptionsResolver interface {
	Dump(config.JobConfig) (dump.Request, error)
	Storage(config.JobConfig) (storage.Options, error)
}

// Dump maps a job to a mysqldump request and resolves its password now.
func (r Resolver) Dump(job config.JobConfig) (dump.Request, error) {
	password, err := r.resolveSecret(job.MySQL.PasswordRef(), job.RunAs)
	if err != nil {
		return dump.Request{}, fmt.Errorf("resolve MySQL password: %w", err)
	}
	return dump.Request{
		Binary:       job.DumpBinary,
		RunAs:        job.RunAs,
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

	accessKeyID, err := r.resolveSecret(job.S3.AccessKeyIDRef(), job.RunAs)
	if err != nil {
		return storage.Options{}, fmt.Errorf("resolve S3 access key ID: %w", err)
	}
	secretAccessKey, err := r.resolveSecret(job.S3.SecretAccessKeyRef(), job.RunAs)
	if err != nil {
		return storage.Options{}, fmt.Errorf("resolve S3 secret access key: %w", err)
	}
	sessionToken, err := r.resolveSecret(job.S3.SessionTokenRef(), job.RunAs)
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

func (r Resolver) resolveSecret(reference config.SecretRef, runAs string) (value string, returnErr error) {
	if reference.Literal != "" && reference.File != "" {
		return "", errors.New("literal and file secret sources are mutually exclusive")
	}
	if reference.File == "" {
		return reference.Literal, nil
	}
	if r.readFile != nil {
		return reference.Resolve(r.readFile)
	}
	identity, err := securepath.CurrentIdentity()
	if runAs != "" {
		identity, err = securepath.LookupIdentity(runAs)
	}
	if err != nil {
		return "", err
	}
	file, source, err := securepath.OpenRegular(reference.File, securepath.Policy{
		Label:                  "secret file",
		Identity:               identity,
		Access:                 securepath.AccessRead,
		MaxBytes:               config.MaxSecretFileBytes,
		RequireSingleLink:      true,
		RejectOtherPermissions: true,
	})
	if err != nil {
		return "", err
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close secret file: %w", closeErr))
		}
	}()
	if r.afterSecretOpen != nil {
		r.afterSecretOpen(file)
	}
	data, err := securepath.ReadAll(file, source, config.MaxSecretFileBytes)
	if err != nil {
		return "", err
	}
	defer clear(data)
	trimmed := bytes.TrimRight(data, "\r\n")
	return string(trimmed), nil
}
