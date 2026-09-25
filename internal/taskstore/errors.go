package taskstore

import (
	"fmt"

	"agent-gateway/internal/protocol"
)

// Every failure a caller can act on wraps exactly one of the protocol
// sentinels, so callers branch with errors.Is and never on message text.
//
// Messages are written for operators: they may name identifiers the caller
// already supplied, but never a lease token, a payload, SQL text or file
// contents. A wrong credential produces the same shape of message whether the
// attempt exists or not, so the error text cannot be used to probe for
// attempts.

func invalidf(format string, args ...any) error {
	return wrap(protocol.ErrInvalid, format, args...)
}

func notFoundf(format string, args ...any) error {
	return wrap(protocol.ErrNotFound, format, args...)
}

func conflictf(format string, args ...any) error {
	return wrap(protocol.ErrConflict, format, args...)
}

func unauthorizedf(format string, args ...any) error {
	return wrap(protocol.ErrUnauthorized, format, args...)
}

func expiredf(format string, args ...any) error {
	return wrap(protocol.ErrLeaseExpired, format, args...)
}

func wrap(sentinel error, format string, args ...any) error {
	return fmt.Errorf("taskstore: %s: %w", fmt.Sprintf(format, args...), sentinel)
}

// dbError labels a storage failure. The cause stays attached for logs and for
// errors.Is (context cancellation and driver codes), but the label never
// carries the statement or its arguments.
func dbError(op string, err error) error {
	return fmt.Errorf("taskstore: %s: %w", op, err)
}
