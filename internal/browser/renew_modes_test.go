package browser

import (
	"context"
	"errors"
	"testing"
)

func TestNativeRenewalFallsBackOnceAndHonorsCancellation(t *testing.T) {
	for _, tc := range []struct {
		native bool
		err    error
		calls  int
	}{{true, ErrReauthenticationRequired, 2}, {true, errors.New("native callback aborted"), 2}, {true, context.Canceled, 1}, {true, context.DeadlineExceeded, 1}, {true, ErrClosed, 1}, {false, ErrNotAuthenticated, 1}, {true, nil, 1}} {
		calls := []bool{}
		_, _ = renewModes(tc.native, func(native bool) (*Result, error) { calls = append(calls, native); return nil, tc.err })
		if len(calls) != tc.calls || len(calls) == 2 && calls[1] {
			t.Fatal("invalid renewal retry policy", calls)
		}
	}
}
