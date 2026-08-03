package usage

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A trimmed real --raw response (the codename windows are intentionally present
// to prove we ignore them and read only the stable keys).
const rawSample = `{
  "five_hour":        {"utilization": 7.0,  "resets_at": "2026-06-08T16:10:00.243011+00:00"},
  "seven_day":        {"utilization": 23.0, "resets_at": "2026-06-10T20:59:59.243039+00:00"},
  "seven_day_sonnet": {"utilization": 0.0,  "resets_at": null},
  "seven_day_opus":   null,
  "seven_day_omelette": {"utilization": 0.0, "resets_at": null},
  "extra_usage":      {"is_enabled": false}
}`

func TestParseReadsStableKeys(t *testing.T) {
	u, err := parse([]byte(rawSample))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.Session.Percent != 7 {
		t.Errorf("session = %d, want 7", u.Session.Percent)
	}
	if u.Weekly.Percent != 23 {
		t.Errorf("weekly = %d, want 23", u.Weekly.Percent)
	}
	if !u.Session.HasReset || !u.Weekly.HasReset {
		t.Errorf("expected reset times to parse: session=%v weekly=%v", u.Session.HasReset, u.Weekly.HasReset)
	}
}

func TestToMeterRoundsAndClamps(t *testing.T) {
	v := func(f float64) *float64 { return &f }
	cases := []struct {
		in   *window
		want int
	}{
		{nil, 0},
		{&window{Utilization: nil}, 0},
		{&window{Utilization: v(0.4)}, 0},
		{&window{Utilization: v(2.5)}, 3}, // round-half-away-from-zero (math.Round)
		{&window{Utilization: v(99.6)}, 100},
		{&window{Utilization: v(150)}, 100}, // clamp
	}
	for _, c := range cases {
		if got := toMeter(c.in).Percent; got != c.want {
			t.Errorf("toMeter(%v).Percent = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestRateLimitedIsMatchable pins the contract main.go relies on: a 429 must be
// recognisable as rate limiting, and must not read as a generic failure or as an
// expired token.
func TestRateLimitedIsMatchable(t *testing.T) {
	err := error(RateLimited{RetryAfter: 90 * time.Second})

	if !errors.Is(err, ErrRateLimited) {
		t.Error("errors.Is(err, ErrRateLimited) = false")
	}
	if errors.Is(err, ErrTokenExpired) {
		t.Error("a rate limit must not look like an expired token")
	}

	var rl RateLimited
	if !errors.As(err, &rl) || rl.RetryAfter != 90*time.Second {
		t.Errorf("errors.As gave RetryAfter %v, want 1m30s", rl.RetryAfter)
	}
	if got := err.Error(); !strings.Contains(got, "1m30s") {
		t.Errorf("Error() = %q, want it to mention the delay", got)
	}
	if got := (RateLimited{}).Error(); got != "rate limited" {
		t.Errorf("Error() with no delay = %q, want %q", got, "rate limited")
	}
}

func TestRetryAfter(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"0", 0}, // what this endpoint actually sends
		{"-5", 0},
		{"30", 30 * time.Second},
		{" 120 ", 2 * time.Minute},
		{"not a number", 0},
		{"Thu, 01 Jan 1970 00:00:01 GMT", 0}, // a date in the past
	}
	for _, tt := range tests {
		if got := retryAfter(tt.in); got != tt.want {
			t.Errorf("retryAfter(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}

	// A date in the future should come back as a positive wait.
	future := time.Now().Add(3 * time.Minute).UTC().Format(http.TimeFormat)
	if got := retryAfter(future); got <= 2*time.Minute || got > 3*time.Minute {
		t.Errorf("retryAfter(%q) = %v, want just under 3m", future, got)
	}
}

// TestParseRateLimitEnvelope covers the belt-and-braces path: the same error
// envelope arriving with a non-429 status.
func TestParseRateLimitEnvelope(t *testing.T) {
	body := `{"type":"error","error":{"type":"rate_limit_error","message":"Rate limited. Please try again later."}}`
	if _, err := parse([]byte(body)); !errors.Is(err, ErrRateLimited) {
		t.Errorf("parse(rate_limit_error) err = %v, want ErrRateLimited", err)
	}
}
