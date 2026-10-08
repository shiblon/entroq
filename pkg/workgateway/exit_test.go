package workgateway

import (
	"errors"
	"fmt"
	"testing"
)

// TestExitClassCodes locks the class -> exit code and wire token mapping. A
// supervisor keys its restart policy on the code and a client branches on the
// token, so both are contracts rather than details.
func TestExitClassCodes(t *testing.T) {
	for _, tc := range []struct {
		class ExitClass
		code  int
		token string
	}{
		{ExitOK, 0, "ok"},
		{ExitTransient, 75, "transient"},
		{ExitCaller, 78, "caller"},
		{ExitGateway, 70, "gateway"},
	} {
		if got := tc.class.ExitCode(); got != tc.code {
			t.Errorf("%v.ExitCode() = %d, want %d", tc.class, got, tc.code)
		}
		if got := tc.class.String(); got != tc.token {
			t.Errorf("%v.String() = %q, want %q", tc.class, got, tc.token)
		}
	}
}

// TestAsExitFindsAWrappedClass checks that a transport reads the class through
// however many layers of fmt.Errorf wrapping lie between it and the stop.
func TestAsExitFindsAWrappedClass(t *testing.T) {
	cause := errors.New("backend down")
	wrapped := fmt.Errorf("worker loop: %w", fmt.Errorf("gateway: %w",
		&ExitError{Class: ExitTransient, err: cause}))

	ee, ok := AsExit(wrapped)
	if !ok {
		t.Fatalf("AsExit(%v) found no class", wrapped)
	}
	if ee.Class != ExitTransient {
		t.Errorf("class = %v, want %v", ee.Class, ExitTransient)
	}
	if !errors.Is(ee, cause) {
		t.Errorf("the cause %v did not survive to the ExitError", cause)
	}
}

// TestAsExitIgnoresOtherErrors keeps a plain error from being read as a clean
// stop: ExitOK is zero, so a false positive here would turn a failure into a
// successful exit code.
func TestAsExitIgnoresOtherErrors(t *testing.T) {
	for _, err := range []error{nil, errors.New("surprise"), fmt.Errorf("wrapped: %w", errors.New("surprise"))} {
		if ee, ok := AsExit(err); ok {
			t.Errorf("AsExit(%v) = (%v, true), want no class", err, ee.Class)
		}
	}
}
