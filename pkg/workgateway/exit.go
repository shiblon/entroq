package workgateway

import "errors"

// ExitClass is the shared vocabulary for why the gateway stopped, or for a
// non-fatal error it reports mid-session. One small set of classes, surfaced
// three ways -- as the class field of an error message while the worker runs, as
// a process exit code over a pipe, and as a status for an HTTP transport -- so a
// client branches on the class and never has to interpret a raw code or a Go
// error type. It is the wire analog of a Go worker inspecting an error with
// errors.As.
type ExitClass int

const (
	// ExitOK is a clean stop: graceful shutdown, or the client simply hung up.
	// Nothing is wrong; do not restart on this account.
	ExitOK ExitClass = iota
	// ExitTransient is a backend blip (EntroQ unreachable, being restarted or
	// relocated). Retry or reconnect; it will likely recover.
	ExitTransient
	// ExitCaller is a caller fault: a bad registration, a protocol violation, or
	// a worker-requested fatal, or a handler error the worker reported.
	// Retrying replays the same problem, so stop and surface it for a human to
	// fix.
	ExitCaller
	// ExitGateway is an unexpected gateway-internal error. Stop and surface; it is
	// likely a bug to report rather than something a retry will fix.
	ExitGateway
)

// String is the class token used in an error message's "class" field. ExitOK has
// no token because a clean stop is never reported as an error.
func (c ExitClass) String() string {
	switch c {
	case ExitTransient:
		return "transient"
	case ExitCaller:
		return "caller"
	case ExitGateway:
		return "gateway"
	default:
		return "ok"
	}
}

// ExitCode is the process exit code for the class over a pipe, drawn from the
// sysexits.h conventions so existing tooling recognizes it: 0 clean, 75
// EX_TEMPFAIL (transient, retryable), 78 EX_CONFIG (caller fault), 70 EX_SOFTWARE
// (gateway fault). A supervisor keys its restart policy on these.
func (c ExitClass) ExitCode() int {
	switch c {
	case ExitTransient:
		return 75
	case ExitCaller:
		return 78
	case ExitGateway:
		return 70
	default:
		return 0
	}
}

// ExitError carries the class of a non-clean stop out to a transport, so it can
// map it to a pipe exit code or an HTTP status. A clean stop is a nil error; an
// *ExitError is everything else.
type ExitError struct {
	Class ExitClass
	err   error
}

// Error implements the error interface.
func (e *ExitError) Error() string { return e.err.Error() }

// Unwrap exposes the underlying cause.
func (e *ExitError) Unwrap() error { return e.err }

// AsExit reports whether err is an *ExitError and returns it, so a transport can
// read the class. A nil error or a non-ExitError yields (nil, false).
func AsExit(err error) (*ExitError, bool) {
	var e *ExitError
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
