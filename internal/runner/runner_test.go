package runner

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestRunErr(t *testing.T) {
	signal := errors.New("terminated signal received")
	cases := []struct {
		name        string
		cause       error // nil: context still live
		interrupted bool
		lockLost    bool
	}{
		{name: "live"},
		{name: "signal", cause: signal, interrupted: true},
		{name: "plain cancel", cause: context.Canceled, interrupted: true},
		{name: "lock lost", cause: ErrLockLost, lockLost: true},
		{name: "lock lost wrapped", cause: fmt.Errorf("%w: server unreachable", ErrLockLost), lockLost: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			if tc.cause != nil {
				cancel(tc.cause)
			}
			err := (&Run{ctx: ctx}).Err()
			var ie *InterruptedError
			if got := errors.As(err, &ie); got != tc.interrupted {
				t.Errorf("Err() = %v, interrupted %v, want %v", err, got, tc.interrupted)
			}
			if got := errors.Is(err, ErrLockLost); got != tc.lockLost {
				t.Errorf("Err() = %v, lock lost %v, want %v", err, got, tc.lockLost)
			}
			if tc.cause == nil && err != nil {
				t.Errorf("Err() = %v on a live run", err)
			}
			if tc.interrupted && !errors.Is(err, tc.cause) {
				t.Errorf("Err() = %v does not wrap the cause %v", err, tc.cause)
			}
		})
	}
}
