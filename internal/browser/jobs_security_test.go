package browser

import (
	"context"
	"testing"
	"time"
)

func TestCompletedJobCannotBeClaimedAfterTTLOrCancellation(t *testing.T) {
	for _, expire := range []bool{true, false} {
		jobs := NewJobs(context.Background(), runner(func(context.Context, func(Update)) (*Result, error) {
			return &Result{UID: "private", Credentials: map[string]any{"cookie": "secret"}}, nil
		}))
		state, capability, err := jobs.Start()
		if err != nil {
			t.Fatal(err)
		}
		awaitState(t, jobs, state.ID, capability, "complete")
		if expire {
			jobs.mu.Lock()
			jobs.entries[state.ID].created = time.Now().Add(-jobs.TTL)
			jobs.mu.Unlock()
		} else {
			if err := jobs.Cancel(state.ID); err != nil {
				t.Fatal(err)
			}
		}
		if result, err := jobs.Claim(state.ID, capability); err == nil || result != nil {
			t.Fatal("expired/cancelled credentials claimed")
		}
		jobs.mu.Lock()
		retained := jobs.entries[state.ID].result != nil
		jobs.mu.Unlock()
		if retained {
			t.Fatal("secret retained")
		}
		jobs.Close()
	}
}
