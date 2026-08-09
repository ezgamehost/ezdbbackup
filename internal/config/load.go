package config

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Load reads and decodes a configuration file.
func Load(path string) (*Config, Findings) {
	f, err := os.Open(path)
	if err != nil {
		return nil, Findings{{Path: path, Message: err.Error()}}
	}
	defer f.Close()
	return Decode(f)
}

// Decode strictly decodes exactly one YAML configuration document.
func Decode(r io.Reader) (*Config, Findings) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, Findings{{Message: fmt.Sprintf("read configuration: %v", err)}}
	}

	decoder := yaml.NewDecoder(bytes.NewReader(b))
	decoder.KnownFields(true)
	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		return nil, Findings{{Message: fmt.Sprintf("decode configuration: %v", err)}}
	}
	var additional yaml.Node
	if err := decoder.Decode(&additional); err != io.EOF {
		if err != nil {
			return nil, Findings{{Message: fmt.Sprintf("decode configuration: %v", err)}}
		}
		return nil, Findings{{Message: "configuration must contain exactly one YAML document"}}
	}

	presence, err := defaultFieldPresence(b, cfg.JobNames())
	if err != nil {
		return nil, Findings{{Message: fmt.Sprintf("decode configuration: %v", err)}}
	}
	applyDefaults(&cfg, presence)
	normalizePrefixes(&cfg)
	return &cfg, nil
}

type defaultPresence struct {
	rotationMaxSizeMB  bool
	rotationMaxFiles   bool
	rotationMaxAgeDays bool
	rotationCompress   bool
	mysqlPorts         map[string]bool
}

func defaultFieldPresence(data []byte, jobNames []string) (defaultPresence, error) {
	presence := defaultPresence{mysqlPorts: make(map[string]bool, len(jobNames))}
	paths := []struct {
		path  []string
		found *bool
	}{
		{path: []string{"logging", "rotation", "max_size_mb"}, found: &presence.rotationMaxSizeMB},
		{path: []string{"logging", "rotation", "max_files"}, found: &presence.rotationMaxFiles},
		{path: []string{"logging", "rotation", "max_age_days"}, found: &presence.rotationMaxAgeDays},
		{path: []string{"logging", "rotation", "compress"}, found: &presence.rotationCompress},
	}
	for _, item := range paths {
		found, err := yamlPathExists(data, item.path...)
		if err != nil {
			return defaultPresence{}, err
		}
		*item.found = found
	}
	for _, name := range jobNames {
		found, err := yamlPathExists(data, "jobs", name, "mysql", "port")
		if err != nil {
			return defaultPresence{}, err
		}
		presence.mysqlPorts[name] = found
	}
	return presence, nil
}

func applyDefaults(cfg *Config, presence defaultPresence) {
	if cfg.Defaults.DumpBinary == "" {
		cfg.Defaults.DumpBinary = defaultDumpBinary
	}
	if cfg.Defaults.TempDir == "" {
		cfg.Defaults.TempDir = defaultTempDir
	}
	if cfg.Logging.Directory == "" {
		cfg.Logging.Directory = defaultLogDir
	}
	if !presence.rotationMaxSizeMB {
		cfg.Logging.Rotation.MaxSizeMB = 100
	}
	if !presence.rotationMaxFiles {
		cfg.Logging.Rotation.MaxFiles = 7
	}
	if !presence.rotationMaxAgeDays {
		cfg.Logging.Rotation.MaxAgeDays = 30
	}
	if !presence.rotationCompress {
		cfg.Logging.Rotation.Compress = true
	}
	for name, job := range cfg.Jobs {
		if job.DumpBinary == "" {
			job.DumpBinary = cfg.Defaults.DumpBinary
		}
		if job.TempDir == "" {
			job.TempDir = cfg.Defaults.TempDir
		}
		if !presence.mysqlPorts[name] {
			job.MySQL.Port = 3306
		}
		cfg.Jobs[name] = job
	}
}

func normalizePrefixes(cfg *Config) {
	for name, job := range cfg.Jobs {
		parts := strings.FieldsFunc(job.S3.Prefix, func(r rune) bool { return r == '/' })
		job.S3.Prefix = strings.Join(parts, "/")
		cfg.Jobs[name] = job
	}
}

func yamlPathExists(data []byte, path ...string) (bool, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return false, err
	}
	if len(document.Content) == 0 {
		return false, nil
	}
	node := document.Content[0]
	for _, key := range path {
		if node.Kind != yaml.MappingNode {
			return false, nil
		}
		found := false
		for i := 0; i < len(node.Content); i += 2 {
			if node.Content[i].Value == key {
				node = node.Content[i+1]
				found = true
				break
			}
		}
		if !found {
			return false, nil
		}
	}
	return true, nil
}
