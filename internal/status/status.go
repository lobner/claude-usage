// Package status reads Anthropic's public service-status page
// (https://status.claude.com) through its Statuspage v2 "summary" API and
// reports whether anything other than "All Systems Operational" is going on.
//
// The summary document carries everything the menu needs in one request: the
// overall indicator/description, the unresolved incidents (each with its update
// history, newest first) and the per-component states. No authentication is
// required — this is the same JSON the public status page itself renders.
package status

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultURL is the Statuspage summary document for status.claude.com.
const DefaultURL = "https://status.claude.com/api/v2/summary.json"

const userAgent = "claude-usage/1.0 (+https://github.com/lobner/claude-usage)"

// Incident is an unresolved incident or an in-progress maintenance.
type Incident struct {
	ID           string
	Name         string
	Status       string    // e.g. "investigating", "identified", "in_progress"
	Impact       string    // "none", "minor", "major", "critical"
	URL          string    // permalink on the status page
	LatestUpdate string    // body of the newest update, if any
	UpdatedAt    time.Time // zero if unknown
	Maintenance  bool
}

// Component is a single service on the status page (e.g. "Claude API").
type Component struct {
	Name   string
	Status string // "operational", "degraded_performance", "partial_outage", …
}

// Operational reports whether this component is in its normal state.
func (c Component) Operational() bool { return c.Status == "" || c.Status == "operational" }

// Summary is the parsed status page: the headline indicator plus the detail the
// dropdown shows.
type Summary struct {
	Indicator   string // "none", "minor", "major", "critical", "maintenance"
	Description string // e.g. "All Systems Operational"
	Incidents   []Incident
	Affected    []Component // components that are not operational
}

// AllOperational reports whether the status page says everything is fine. An
// empty indicator is treated as fine so an unexpected payload never raises a
// false alarm.
func (s Summary) AllOperational() bool {
	return s.Indicator == "" || s.Indicator == "none"
}

var httpClient = &http.Client{Timeout: 15 * time.Second}

// Fetch retrieves and parses the status summary from rawURL (use DefaultURL).
func Fetch(ctx context.Context, rawURL string) (Summary, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return Summary{}, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return Summary{}, fmt.Errorf("status request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Summary{}, fmt.Errorf("status page HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // cap at 4 MiB
	if err != nil {
		return Summary{}, fmt.Errorf("reading status page: %w", err)
	}
	return parse(body)
}

type summaryJSON struct {
	Status struct {
		Indicator   string `json:"indicator"`
		Description string `json:"description"`
	} `json:"status"`
	Components []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Group  bool   `json:"group"`
	} `json:"components"`
	Incidents             []incidentJSON `json:"incidents"`
	ScheduledMaintenances []incidentJSON `json:"scheduled_maintenances"`
}

type incidentJSON struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Status          string `json:"status"`
	Impact          string `json:"impact"`
	Shortlink       string `json:"shortlink"`
	UpdatedAt       string `json:"updated_at"`
	IncidentUpdates []struct {
		Body      string `json:"body"`
		Status    string `json:"status"`
		CreatedAt string `json:"created_at"`
	} `json:"incident_updates"`
}

func parse(body []byte) (Summary, error) {
	var raw summaryJSON
	if err := json.Unmarshal(body, &raw); err != nil {
		return Summary{}, fmt.Errorf("parsing status JSON: %w", err)
	}

	s := Summary{
		Indicator:   strings.TrimSpace(raw.Status.Indicator),
		Description: strings.TrimSpace(raw.Status.Description),
	}
	for _, in := range raw.Incidents {
		if isResolved(in.Status) {
			continue
		}
		s.Incidents = append(s.Incidents, toIncident(in, false))
	}
	for _, m := range raw.ScheduledMaintenances {
		// Only maintenance that is actually running matters here; upcoming
		// windows aren't a disruption yet.
		if m.Status != "in_progress" && m.Status != "verifying" {
			continue
		}
		s.Incidents = append(s.Incidents, toIncident(m, true))
	}
	for _, c := range raw.Components {
		// Group rows only mirror their children's worst state.
		if c.Group {
			continue
		}
		comp := Component{Name: strings.TrimSpace(c.Name), Status: strings.TrimSpace(c.Status)}
		if comp.Name == "" || comp.Operational() {
			continue
		}
		s.Affected = append(s.Affected, comp)
	}
	return s, nil
}

func isResolved(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "resolved", "postmortem", "completed":
		return true
	default:
		return false
	}
}

func toIncident(in incidentJSON, maintenance bool) Incident {
	inc := Incident{
		ID:          strings.TrimSpace(in.ID),
		Name:        strings.TrimSpace(in.Name),
		Status:      strings.TrimSpace(in.Status),
		Impact:      strings.TrimSpace(in.Impact),
		URL:         strings.TrimSpace(in.Shortlink),
		Maintenance: maintenance,
	}
	if len(in.IncidentUpdates) > 0 {
		// Statuspage lists updates newest-first.
		inc.LatestUpdate = collapseSpace(in.IncidentUpdates[0].Body)
	}
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(in.UpdatedAt)); err == nil {
		inc.UpdatedAt = t
	}
	return inc
}

// collapseSpace flattens an update body to a single line so it fits a menu row
// or a tooltip.
func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// Human turns a Statuspage machine value ("degraded_performance",
// "in_progress") into a display label ("Degraded performance", "In progress").
func Human(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "_", " "))
	if s == "" {
		return ""
	}
	r := []rune(s)
	return strings.ToUpper(string(r[0])) + string(r[1:])
}
