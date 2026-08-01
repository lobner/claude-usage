package status

import "testing"

// A trimmed real-shaped summary.json with an ongoing incident, a resolved one
// (which must be ignored), an upcoming maintenance (also ignored) and a mix of
// component states.
const outageSample = `{
  "page": {"id": "abc", "name": "Anthropic"},
  "components": [
    {"name": "Claude API", "status": "partial_outage", "group": false},
    {"name": "claude.ai",  "status": "operational",    "group": false},
    {"name": "Models",     "status": "degraded_performance", "group": false},
    {"name": "Everything", "status": "partial_outage", "group": true}
  ],
  "incidents": [
    {
      "id": "inc1",
      "name": "Elevated errors on the Claude API",
      "status": "investigating",
      "impact": "major",
      "shortlink": "https://stspg.io/inc1",
      "updated_at": "2026-06-08T16:10:00.000Z",
      "incident_updates": [
        {"body": "We are\n investigating   elevated error rates.", "status": "investigating"},
        {"body": "Older update", "status": "investigating"}
      ]
    },
    {
      "id": "inc2",
      "name": "Old incident",
      "status": "resolved",
      "impact": "minor",
      "shortlink": "https://stspg.io/inc2",
      "incident_updates": [{"body": "Resolved.", "status": "resolved"}]
    }
  ],
  "scheduled_maintenances": [
    {"id": "m1", "name": "Upcoming database upgrade", "status": "scheduled"},
    {"id": "m2", "name": "Console maintenance", "status": "in_progress", "shortlink": "https://stspg.io/m2"}
  ],
  "status": {"indicator": "major", "description": "Partial System Outage"}
}`

const operationalSample = `{
  "components": [{"name": "Claude API", "status": "operational", "group": false}],
  "incidents": [],
  "scheduled_maintenances": [],
  "status": {"indicator": "none", "description": "All Systems Operational"}
}`

func TestParseOperational(t *testing.T) {
	s, err := parse([]byte(operationalSample))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !s.AllOperational() {
		t.Errorf("AllOperational() = false, want true (indicator %q)", s.Indicator)
	}
	if len(s.Incidents) != 0 || len(s.Affected) != 0 {
		t.Errorf("expected no incidents/affected components, got %d/%d", len(s.Incidents), len(s.Affected))
	}
	if s.Description != "All Systems Operational" {
		t.Errorf("description = %q", s.Description)
	}
}

func TestParseOutage(t *testing.T) {
	s, err := parse([]byte(outageSample))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s.AllOperational() {
		t.Errorf("AllOperational() = true, want false (indicator %q)", s.Indicator)
	}

	if len(s.Incidents) != 2 {
		t.Fatalf("incidents = %d, want 2 (unresolved incident + in-progress maintenance)", len(s.Incidents))
	}
	inc := s.Incidents[0]
	if inc.Name != "Elevated errors on the Claude API" || inc.Status != "investigating" {
		t.Errorf("incident = %+v", inc)
	}
	if inc.URL != "https://stspg.io/inc1" {
		t.Errorf("incident URL = %q", inc.URL)
	}
	if inc.LatestUpdate != "We are investigating elevated error rates." {
		t.Errorf("latest update = %q (want newest update, whitespace collapsed)", inc.LatestUpdate)
	}
	if inc.UpdatedAt.IsZero() {
		t.Errorf("expected updated_at to parse")
	}
	if m := s.Incidents[1]; !m.Maintenance || m.Name != "Console maintenance" {
		t.Errorf("second entry = %+v, want the in-progress maintenance", m)
	}

	if len(s.Affected) != 2 {
		t.Fatalf("affected = %+v, want the two non-operational non-group components", s.Affected)
	}
	if s.Affected[0].Name != "Claude API" || s.Affected[1].Name != "Models" {
		t.Errorf("affected = %+v", s.Affected)
	}
}

func TestAllOperationalTreatsUnknownAsFine(t *testing.T) {
	if !(Summary{}).AllOperational() {
		t.Errorf("empty summary should not raise an alarm")
	}
	for _, ind := range []string{"minor", "major", "critical", "maintenance"} {
		if (Summary{Indicator: ind}).AllOperational() {
			t.Errorf("indicator %q should not be operational", ind)
		}
	}
}

func TestHuman(t *testing.T) {
	cases := map[string]string{
		"degraded_performance": "Degraded performance",
		"in_progress":          "In progress",
		"":                     "",
	}
	for in, want := range cases {
		if got := Human(in); got != want {
			t.Errorf("Human(%q) = %q, want %q", in, got, want)
		}
	}
}
