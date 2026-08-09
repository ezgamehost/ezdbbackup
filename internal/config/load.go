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

	compressConfigured, err := yamlPathExists(b, "logging", "rotation", "compress")
	if err != nil {
		return nil, Findings{{Message: fmt.Sprintf("decode configuration: %v", err)}}
	}
	applyDefaults(&cfg, compressConfigured)
	normalizePrefixes(&cfg)
	return &cfg, nil
}

func applyDefaults(cfg *Config, compressConfigured bool) {
	if cfg.Defaults.DumpBinary == "" {
		cfg.Defaults.DumpBinary = defaultDumpBinary
	}
	if cfg.Defaults.TempDir == "" {
		cfg.Defaults.TempDir = defaultTempDir
	}
	if cfg.Logging.Directory == "" {
		cfg.Logging.Directory = defaultLogDir
	}
	if cfg.Logging.Rotation.MaxSizeMB == 0 {
		cfg.Logging.Rotation.MaxSizeMB = 100
	}
	if cfg.Logging.Rotation.MaxFiles == 0 {
		cfg.Logging.Rotation.MaxFiles = 7
	}
	if cfg.Logging.Rotation.MaxAgeDays == 0 {
		cfg.Logging.Rotation.MaxAgeDays = 30
	}
	if !compressConfigured {
		cfg.Logging.Rotation.Compress = true
	}
	for name, job := range cfg.Jobs {
		if job.DumpBinary == "" {
			job.DumpBinary = cfg.Defaults.DumpBinary
		}
		if job.TempDir == "" {
			job.TempDir = cfg.Defaults.TempDir
		}
		if job.MySQL.Port == 0 {
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
