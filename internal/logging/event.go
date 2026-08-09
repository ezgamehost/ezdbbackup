package logging

import "time"

type Sink interface {
	Write(Event) error
}

type Level string

const (
	DebugLevel Level = "debug"
	InfoLevel  Level = "info"
	WarnLevel  Level = "warn"
	ErrorLevel Level = "error"
)

type Event struct {
	Time    time.Time      `json:"timestamp"`
	Level   Level          `json:"level"`
	Message string         `json:"message"`
	Command string         `json:"command"`
	Job     string         `json:"job,omitempty"`
	Stage   string         `json:"stage,omitempty"`
	Fields  map[string]any `json:"-"`
}

type Options struct {
	Directory string
	Debug     bool
	Rotation  RotationOptions
}

type RotationOptions struct {
	MaxSizeBytes int64
	MaxFiles     int
	MaxAge       time.Duration
	Compress     bool
}
