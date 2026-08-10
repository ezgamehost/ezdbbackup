// Package config loads and validates ezdbbackup configuration files.
package config

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ezgamehost/ezdbbackup/internal/securepath"
)

const (
	defaultDumpBinary = "/usr/bin/mysqldump"
	defaultTempDir    = "/var/lib/ezdbbackup/tmp"
	defaultLogDir     = "/var/log/ezdbbackup"
)

// Config is the versioned ezdbbackup configuration document.
type Config struct {
	Version  int                  `yaml:"version"`
	Defaults Defaults             `yaml:"defaults"`
	Logging  LoggingConfig        `yaml:"logging"`
	Jobs     map[string]JobConfig `yaml:"jobs"`
	source   *securepath.Source
}

type Defaults struct {
	DumpBinary string `yaml:"dump_binary"`
	TempDir    string `yaml:"temp_dir"`
}

type LoggingConfig struct {
	Directory string         `yaml:"directory"`
	Debug     bool           `yaml:"debug"`
	Rotation  RotationConfig `yaml:"rotation"`
}

type RotationConfig struct {
	MaxSizeMB  int  `yaml:"max_size_mb"`
	MaxFiles   int  `yaml:"max_files"`
	MaxAgeDays int  `yaml:"max_age_days"`
	Compress   bool `yaml:"compress"`
}

type JobConfig struct {
	Enabled    bool        `yaml:"enabled"`
	Schedule   string      `yaml:"schedule"`
	RunAs      string      `yaml:"run_as"`
	DumpBinary string      `yaml:"dump_binary"`
	TempDir    string      `yaml:"temp_dir"`
	MySQL      MySQLConfig `yaml:"mysql"`
	S3         S3Config    `yaml:"s3"`
}

type MySQLConfig struct {
	Host         string            `yaml:"host"`
	Port         int               `yaml:"port"`
	User         string            `yaml:"user"`
	Password     string            `yaml:"password"`
	PasswordFile string            `yaml:"password_file"`
	Databases    DatabaseSelection `yaml:"databases"`
	ExtraArgs    []string          `yaml:"extra_args"`
}

type S3Config struct {
	Bucket              string `yaml:"bucket"`
	Prefix              string `yaml:"prefix"`
	Region              string `yaml:"region"`
	Endpoint            string `yaml:"endpoint"`
	ForcePathStyle      bool   `yaml:"force_path_style"`
	AccessKeyID         string `yaml:"access_key_id"`
	AccessKeyIDFile     string `yaml:"access_key_id_file"`
	SecretAccessKey     string `yaml:"secret_access_key"`
	SecretAccessKeyFile string `yaml:"secret_access_key_file"`
	SessionToken        string `yaml:"session_token"`
	SessionTokenFile    string `yaml:"session_token_file"`
}

// Finding is one configuration error or warning. Path uses dot notation.
type Finding struct {
	Path    string
	Job     string
	Message string
	Warning bool
}

func (f Finding) String() string {
	severity := "error"
	if f.Warning {
		severity = "warning"
	}
	if f.Path == "" {
		return fmt.Sprintf("%s: %s", severity, f.Message)
	}
	return fmt.Sprintf("%s %s: %s", severity, f.Path, f.Message)
}

// Findings contains every problem discovered while loading or validating.
type Findings []Finding

func (f Findings) HasErrors() bool {
	for _, finding := range f {
		if !finding.Warning {
			return true
		}
	}
	return false
}

func (f Findings) ContainsWarning(path string) bool {
	for _, finding := range f {
		if finding.Warning && finding.Path == path {
			return true
		}
	}
	return false
}

func (f Findings) Error() string {
	parts := make([]string, len(f))
	for i, finding := range f {
		parts[i] = finding.String()
	}
	return strings.Join(parts, "; ")
}

func (f *Findings) addError(path, message string) {
	f.addJobError("", path, message)
}

func (f *Findings) addWarning(path, message string) {
	f.addJobWarning("", path, message)
}

func (f *Findings) addJobError(job, path, message string) {
	*f = append(*f, Finding{Path: path, Job: job, Message: message})
}

func (f *Findings) addJobWarning(job, path, message string) {
	*f = append(*f, Finding{Path: path, Job: job, Message: message, Warning: true})
}

// JobNames returns all configured job names in lexical order.
func (c *Config) JobNames() []string {
	if c == nil {
		return nil
	}
	names := make([]string, 0, len(c.Jobs))
	for name := range c.Jobs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// EnabledJobNames returns enabled job names in lexical order.
func (c *Config) EnabledJobNames() []string {
	if c == nil {
		return nil
	}
	names := make([]string, 0, len(c.Jobs))
	for name, job := range c.Jobs {
		if job.Enabled {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// Source returns the exact file identity used by Load. Configurations decoded
// from an arbitrary reader intentionally have no filesystem source.
func (c *Config) Source() (securepath.Source, bool) {
	if c == nil || c.source == nil {
		return securepath.Source{}, false
	}
	return *c.source, true
}

// TrustedPath returns the canonical non-replaceable source path when this
// configuration came from Load, or fallback for reader-decoded test values.
func (c *Config) TrustedPath(fallback string) string {
	if source, ok := c.Source(); ok {
		return source.CanonicalPath
	}
	return fallback
}

// RecheckSource proves that the trusted canonical path still names the exact
// file parsed by Load.
func (c *Config) RecheckSource() error {
	if source, ok := c.Source(); ok {
		return securepath.Recheck(source)
	}
	return nil
}
