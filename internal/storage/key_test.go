package storage

import (
	"strings"
	"testing"
	"time"
)

func TestObjectKeyNormalizesPrefixAndUsesUTC(t *testing.T) {
	started := time.Date(2026, 8, 9, 2, 3, 4, 123456789, time.FixedZone("offset", 3600))

	got := ObjectKey("/production//mysql/", "primary", started)
	want := "production/mysql/primary/2026/08/09/primary-20260809T010304.123456789Z.sql.gz"
	if got != want {
		t.Fatalf("ObjectKey() = %q, want %q", got, want)
	}
}

func TestObjectKeyOmitsEmptyPrefixSegment(t *testing.T) {
	started := time.Date(2026, 8, 9, 1, 3, 4, 123456789, time.UTC)

	got := ObjectKey("", "primary", started)
	if !strings.HasPrefix(got, "primary/") {
		t.Fatalf("ObjectKey() = %q, want prefix %q", got, "primary/")
	}
}

func TestObjectKeyDistinguishesNanoseconds(t *testing.T) {
	first := time.Date(2026, 8, 9, 1, 3, 4, 1, time.UTC)
	second := time.Date(2026, 8, 9, 1, 3, 4, 2, time.UTC)

	firstKey := ObjectKey("backups", "primary", first)
	secondKey := ObjectKey("backups", "primary", second)
	if firstKey == secondKey {
		t.Fatalf("ObjectKey() returned duplicate key %q for distinct nanoseconds", firstKey)
	}
}
