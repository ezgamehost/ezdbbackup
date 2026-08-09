package storage

import (
	"fmt"
	"strings"
	"time"
)

const objectTimestampFormat = "20060102T150405.000000000Z"

func ObjectKey(prefix, job string, started time.Time) string {
	prefix = strings.Join(strings.FieldsFunc(prefix, func(r rune) bool {
		return r == '/'
	}), "/")

	started = started.UTC()
	datePath := started.Format("2006/01/02")
	filename := fmt.Sprintf("%s-%s.sql.gz", job, started.Format(objectTimestampFormat))
	parts := []string{job, datePath, filename}
	if prefix != "" {
		parts = append([]string{prefix}, parts...)
	}
	return strings.Join(parts, "/")
}
