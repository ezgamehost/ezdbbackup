// Package terminal renders untrusted values safely for human-facing terminals.
package terminal

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// MaxEncodedBytes is the maximum UTF-8 byte length of one encoded value.
	MaxEncodedBytes = 4096
	TruncatedSuffix = "…[truncated]"
)

// Encode renders one bounded physical-line value. Normal UTF-8 text is left
// unchanged; terminal controls, bidi controls, and invalid bytes are visible.
func Encode(value string) string {
	var output strings.Builder
	bodyLimit := MaxEncodedBytes - len(TruncatedSuffix)
	index := 0
	for index < len(value) {
		token, width := encodedToken(value[index:])
		if output.Len()+len(token) > bodyLimit {
			break
		}
		output.WriteString(token)
		index += width
	}
	if index < len(value) {
		output.WriteString(TruncatedSuffix)
	}
	return output.String()
}

// EncodeLines safely renders intentionally multiline content while preserving
// only literal LF separators between independently bounded lines.
func EncodeLines(value []byte) string {
	var output strings.Builder
	start := 0
	for index, current := range value {
		if current != '\n' {
			continue
		}
		output.WriteString(Encode(string(value[start:index])))
		output.WriteByte('\n')
		start = index + 1
	}
	if start < len(value) {
		output.WriteString(Encode(string(value[start:])))
	}
	return output.String()
}

func encodedToken(value string) (string, int) {
	current, width := utf8.DecodeRuneInString(value)
	if current == utf8.RuneError && width == 1 {
		return fmt.Sprintf(`\x%02x`, value[0]), 1
	}
	switch current {
	case '\n':
		return `\n`, width
	case '\r':
		return `\r`, width
	case '\t':
		return `\t`, width
	}
	if current == utf8.RuneError || unicode.IsControl(current) {
		if current <= 0xff {
			return fmt.Sprintf(`\x%02x`, current), width
		}
		return fmt.Sprintf(`\u%04x`, current), width
	}
	if isBidiControl(current) || current == '\u2028' || current == '\u2029' {
		return fmt.Sprintf(`\u%04x`, current), width
	}
	return value[:width], width
}

func isBidiControl(value rune) bool {
	return value == '\u061c' || value == '\u200e' || value == '\u200f' ||
		(value >= '\u202a' && value <= '\u202e') ||
		(value >= '\u2066' && value <= '\u2069')
}
