package terminal

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestEncodePreservesNormalText(t *testing.T) {
	const input = "backup production: café /var/lib/db"
	if got := Encode(input); got != input {
		t.Fatalf("Encode() = %q, want %q", got, input)
	}
}

func TestEncodeEscapesTerminalControlsBidiAndInvalidUTF8(t *testing.T) {
	input := string([]byte{'a', '\n', '\r', '\t', 0x1b, ']', '0', ';', 'x', 0x07, 0x80}) + "\u202eend"
	got := Encode(input)
	for _, forbidden := range []string{"\n", "\r", "\t", "\x1b", "\x07", "\u202e", string([]byte{0x80})} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("Encode() left unsafe sequence %q in %q", forbidden, got)
		}
	}
	for _, escaped := range []string{`\n`, `\r`, `\t`, `\x1b`, `\x07`, `\x80`, `\u202e`} {
		if !strings.Contains(got, escaped) {
			t.Fatalf("Encode() = %q, want visible escape %q", got, escaped)
		}
	}
	if !utf8.ValidString(got) {
		t.Fatalf("Encode() returned invalid UTF-8: %q", got)
	}
}

func TestEncodeBoundsOutputWithoutSplittingEscapes(t *testing.T) {
	got := Encode(strings.Repeat("\x1b", MaxEncodedBytes))
	if len(got) > MaxEncodedBytes {
		t.Fatalf("encoded length = %d, want <= %d", len(got), MaxEncodedBytes)
	}
	if !strings.HasSuffix(got, TruncatedSuffix) {
		t.Fatalf("Encode() suffix = %q, want %q", got[len(got)-32:], TruncatedSuffix)
	}
	if strings.ContainsRune(got, '\x1b') {
		t.Fatal("bounded output contains raw ESC")
	}
}

func TestEncodeLinesPreservesStructuralNewlinesOnly(t *testing.T) {
	got := EncodeLines([]byte("normal line\nmalicious\rline\x1b]8;;x\n"))
	want := "normal line\nmalicious\\rline\\x1b]8;;x\n"
	if got != want {
		t.Fatalf("EncodeLines() = %q, want %q", got, want)
	}
}
