package config

import (
	"errors"
	"testing"
)

func TestSecretRefResolve(t *testing.T) {
	tests := []struct {
		name string
		ref  SecretRef
		want string
		err  bool
	}{
		{name: "literal", ref: SecretRef{Literal: "value"}, want: "value"},
		{name: "file trims newlines", ref: SecretRef{File: "/secret"}, want: "value"},
		{name: "conflict", ref: SecretRef{Literal: "value", File: "/secret"}, err: true},
		{name: "read error", ref: SecretRef{File: "/missing"}, err: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.ref.Resolve(func(path string) ([]byte, error) {
				if path == "/secret" {
					return []byte("value\r\n"), nil
				}
				return nil, errors.New("missing")
			})
			if (err != nil) != tt.err {
				t.Fatalf("Resolve() error = %v, want error=%v", err, tt.err)
			}
			if got != tt.want {
				t.Fatalf("Resolve() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSecretReferenceAccessors(t *testing.T) {
	mysql := MySQLConfig{Password: "password", PasswordFile: "/password"}
	if got, want := mysql.PasswordRef(), (SecretRef{Literal: "password", File: "/password"}); got != want {
		t.Fatalf("PasswordRef() = %#v, want %#v", got, want)
	}
	s3 := S3Config{AccessKeyID: "id", AccessKeyIDFile: "/id", SecretAccessKey: "secret", SecretAccessKeyFile: "/secret", SessionToken: "token", SessionTokenFile: "/token"}
	if got, want := s3.AccessKeyIDRef(), (SecretRef{Literal: "id", File: "/id"}); got != want {
		t.Fatalf("AccessKeyIDRef() = %#v, want %#v", got, want)
	}
	if got, want := s3.SecretAccessKeyRef(), (SecretRef{Literal: "secret", File: "/secret"}); got != want {
		t.Fatalf("SecretAccessKeyRef() = %#v, want %#v", got, want)
	}
	if got, want := s3.SessionTokenRef(), (SecretRef{Literal: "token", File: "/token"}); got != want {
		t.Fatalf("SessionTokenRef() = %#v, want %#v", got, want)
	}
}
