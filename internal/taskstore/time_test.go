package taskstore

import (
	"testing"
	"time"
)

// TestFormatTimeIsFixedWidthUTC pins the storage format. Lease expiry is
// compared as text inside SQLite, so the format must order the way the
// instants do and must not depend on the location the value was produced in.
func TestFormatTimeIsFixedWidthUTC(t *testing.T) {
	instant := time.Date(2026, 9, 25, 10, 30, 0, 0, time.UTC)
	if got, want := formatTime(instant), "2026-09-25T10:30:00.000000000Z"; got != want {
		t.Fatalf("formatTime = %q, want %q", got, want)
	}
	zone := time.FixedZone("UTC+8", 8*3600)
	if got, want := formatTime(instant.In(zone)), formatTime(instant); got != want {
		t.Fatalf("formatTime depends on the location: %q vs %q", got, want)
	}

	withFraction := instant.Add(500 * time.Millisecond)
	if len(formatTime(withFraction)) != len(formatTime(instant)) {
		t.Fatalf("stored times are not fixed width: %q vs %q", formatTime(withFraction), formatTime(instant))
	}
	if !(formatTime(withFraction) > formatTime(instant)) {
		t.Fatalf("%q does not sort after %q", formatTime(withFraction), formatTime(instant))
	}
	if !(formatTime(instant.Add(-time.Nanosecond)) < formatTime(instant)) {
		t.Fatal("a nanosecond earlier does not sort before")
	}

	parsed, err := parseTime(formatTime(withFraction))
	if err != nil {
		t.Fatalf("parseTime: %v", err)
	}
	if !parsed.Equal(withFraction) {
		t.Fatalf("round trip changed the instant: %s -> %s", withFraction, parsed)
	}
}
