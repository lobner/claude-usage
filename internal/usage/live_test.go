//go:build livetest

// Diagnostic: queries the live usage endpoint with the stored OAuth token and
// prints the two meters. Build-tagged, opt-in (needs a valid Claude Code OAuth
// token).
//
//	go test -tags livetest -run TestLiveFetch -v ./internal/usage
package usage

import (
	"context"
	"testing"
	"time"
)

func TestLiveFetch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	u, err := Fetch(ctx)
	if err == ErrTokenExpired {
		t.Skip("OAuth token expired — run any Claude Code command, then retry")
	}
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	t.Logf("session = %d%% (hasReset=%v, resets %s)", u.Session.Percent, u.Session.HasReset, u.Session.ResetsAt.Local().Format("Mon 15:04"))
	t.Logf("weekly  = %d%% (hasReset=%v, resets %s)", u.Weekly.Percent, u.Weekly.HasReset, u.Weekly.ResetsAt.Local().Format("Mon 15:04"))
	for _, p := range []int{u.Session.Percent, u.Weekly.Percent} {
		if p < 0 || p > 100 {
			t.Errorf("percent out of range: %d", p)
		}
	}
}
