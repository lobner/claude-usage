package usage

import "testing"

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
