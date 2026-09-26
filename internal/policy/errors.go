package policy

import (
	"fmt"

	"agent-gateway/internal/protocol"
)

// Every failure a caller can act on wraps exactly one of the protocol
// sentinels, so callers branch with errors.Is and never on message text.
//
// Messages are written for operators: they may name an identifier the caller
// already supplied, but never a token, a scope it did not ask about, SQL text
// or file contents. The one message that must not be more specific than it is
// is the authentication failure — see unauthorizedTokenf.

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

// unauthorizedTokenf is the single failure an unauthenticated caller can
// observe, whatever the real reason. An unknown, an expired and a revoked
// credential must be indistinguishable from outside: if a token that was once
// valid reported "revoked" while a guessed one reported "unknown", the error
// itself would become an oracle for probing credentials. The three reasons are
// still separable locally — revoked_at and expires_at are in the row — which
// is where an operator diagnoses it.
func unauthorizedTokenf() error {
	return unauthorizedf("token is unknown, revoked or expired")
}

func wrap(sentinel error, format string, args ...any) error {
	return fmt.Errorf("policy: %s: %w", fmt.Sprintf(format, args...), sentinel)
}

// dbError labels a storage failure. The cause stays attached for logs and for
// errors.Is (context cancellation and driver codes), but the label never
// carries the statement or its arguments.
func dbError(op string, err error) error {
	return fmt.Errorf("policy: %s: %w", op, err)
}
