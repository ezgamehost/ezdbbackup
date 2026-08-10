package storage

import (
	"context"
	"io"
)

type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Explicit        bool
}

type Options struct {
	Region         string
	Endpoint       string
	ForcePathStyle bool
	Credentials    Credentials
}

type UploadResult struct {
	Location string
	ETag     string
}

type Store interface {
	UploadFile(context.Context, string, string, io.ReaderAt, int64) (UploadResult, error)
	Probe(context.Context, string) error
}

type Factory interface {
	New(context.Context, Options) (Store, error)
}
