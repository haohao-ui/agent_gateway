package policy

import "time"

// timeLayout is the storage format for every persisted timestamp: UTC, always
// nine fractional digits, always the Z suffix.
//
// The fixed width keeps stored times comparable as text, which is what lets a
// local operator answer "when did this credential expire, and when was it
// revoked?" with a SQL comparison, and makes a dump of the table read the same
// way regardless of the host's timezone.
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

// formatTime renders t in the storage format. The value is converted to UTC
// first, so one instant is always one string regardless of the location it was
// produced in. Times are never handed to the driver as time.Time, which would
// make the persisted form depend on driver options.
func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

// parseTime reads a stored timestamp back.
func parseTime(s string) (time.Time, error) {
	return time.Parse(timeLayout, s)
}
