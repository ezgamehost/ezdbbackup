package config

import (
	"errors"
	"strings"
)

// SecretRef is a secret supplied literally or from a file.
type SecretRef struct {
	Literal string
	File    string
}

func (s SecretRef) Resolve(readFile func(string) ([]byte, error)) (string, error) {
	if s.Literal != "" && s.File != "" {
		return "", errors.New("literal and file secret sources are mutually exclusive")
	}
	if s.File == "" {
		return s.Literal, nil
	}
	b, err := readFile(s.File)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

func (m MySQLConfig) PasswordRef() SecretRef {
	return SecretRef{Literal: m.Password, File: m.PasswordFile}
}

func (s S3Config) AccessKeyIDRef() SecretRef {
	return SecretRef{Literal: s.AccessKeyID, File: s.AccessKeyIDFile}
}

func (s S3Config) SecretAccessKeyRef() SecretRef {
	return SecretRef{Literal: s.SecretAccessKey, File: s.SecretAccessKeyFile}
}

func (s S3Config) SessionTokenRef() SecretRef {
	return SecretRef{Literal: s.SessionToken, File: s.SessionTokenFile}
}
