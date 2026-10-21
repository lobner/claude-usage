//! Reads Anthropic's public service-status page (<https://status.claude.com>)
//! through its Statuspage v2 "summary" API and reports whether anything other
//! than "All Systems Operational" is going on.
//!
//! The summary document carries everything the menu needs in one request: the
//! overall indicator and description, the unresolved incidents (each with its
//! update history, newest first) and the per-component states. No
//! authentication is necessary. This is the same JSON the public status page
//! itself renders.

use std::time::{Duration, Instant};

use chrono::{DateTime, FixedOffset};
use serde::Deserialize;

use crate::http;
use crate::json::nullable;

/// The Statuspage summary document for status.claude.com.
pub const DEFAULT_URL: &str = "https://status.claude.com/api/v2/summary.json";

const USER_AGENT: &str = "claude-usage/1.0 (+https://github.com/lobner/claude-usage)";

const CLIENT_TIMEOUT: Duration = Duration::from_secs(15);
const BODY_LIMIT: u64 = 4 << 20; // 4 MiB

/// An unresolved incident or an in-progress maintenance.
#[derive(Debug, Clone, Default)]
#[allow(dead_code)] // impact and updated_at are parsed for parity, not shown
pub struct Incident {
    pub id: String,
    pub name: String,
    /// For example "investigating", "identified", "in_progress".
    pub status: String,
    /// "none", "minor", "major", "critical".
    pub impact: String,
    /// The permalink on the status page.
    pub url: String,
    /// The body of the newest update, if any.
    pub latest_update: String,
    pub updated_at: Option<DateTime<FixedOffset>>,
    pub maintenance: bool,
}

/// A single service on the status page (for example "Claude API").
#[derive(Debug, Clone, Default)]
pub struct Component {
    pub name: String,
    /// "operational", "degraded_performance", "partial_outage", ...
    pub status: String,
}

impl Component {
    /// Reports whether this component is in its normal state.
    pub fn operational(&self) -> bool {
        self.status.is_empty() || self.status == "operational"
    }
}

/// The parsed status page: the headline indicator plus the detail the dropdown
/// shows.
#[derive(Debug, Clone, Default)]
pub struct Summary {
    /// "none", "minor", "major", "critical", "maintenance".
    pub indicator: String,
    /// For example "All Systems Operational".
    pub description: String,
    pub incidents: Vec<Incident>,
    /// The components that are not operational.
    pub affected: Vec<Component>,
}

impl Summary {
    /// Reports whether the status page says everything is fine. An empty
    /// indicator counts as fine, so an unexpected payload never raises a false
    /// alarm.
    pub fn all_operational(&self) -> bool {
        self.indicator.is_empty() || self.indicator == "none"
    }
}

/// Gets and parses the status summary from `url` (use [`DEFAULT_URL`]).
pub fn fetch(url: &str, deadline: Instant) -> Result<Summary, String> {
    let mut resp = http::agent()
        .get(url)
        .header("User-Agent", USER_AGENT)
        .header("Accept", "application/json")
        .config()
        .timeout_global(Some(http::timeout(CLIENT_TIMEOUT, deadline)))
        .build()
        .call()
        .map_err(|e| format!("status request: {e}"))?;

    if resp.status().as_u16() != 200 {
        return Err(format!("status page HTTP {}", resp.status().as_u16()));
    }
    let body = http::read_limited(resp.body_mut(), BODY_LIMIT)
        .map_err(|e| format!("reading status page: {e}"))?;
    parse(&body)
}

// Every field is optional and may be null, as with Go's encoding/json.

#[derive(Deserialize, Default)]
#[serde(default)]
struct SummaryJson {
    #[serde(deserialize_with = "nullable")]
    status: StatusJson,
    #[serde(deserialize_with = "nullable")]
    components: Vec<ComponentJson>,
    #[serde(deserialize_with = "nullable")]
    incidents: Vec<IncidentJson>,
    #[serde(deserialize_with = "nullable")]
    scheduled_maintenances: Vec<IncidentJson>,
}

#[derive(Deserialize, Default)]
#[serde(default)]
struct StatusJson {
    #[serde(deserialize_with = "nullable")]
    indicator: String,
    #[serde(deserialize_with = "nullable")]
    description: String,
}

#[derive(Deserialize, Default)]
#[serde(default)]
struct ComponentJson {
    #[serde(deserialize_with = "nullable")]
    name: String,
    #[serde(deserialize_with = "nullable")]
    status: String,
    #[serde(deserialize_with = "nullable")]
    group: bool,
}

#[derive(Deserialize, Default)]
#[serde(default)]
struct IncidentJson {
    #[serde(deserialize_with = "nullable")]
    id: String,
    #[serde(deserialize_with = "nullable")]
    name: String,
    #[serde(deserialize_with = "nullable")]
    status: String,
    #[serde(deserialize_with = "nullable")]
    impact: String,
    #[serde(deserialize_with = "nullable")]
    shortlink: String,
    #[serde(deserialize_with = "nullable")]
    updated_at: String,
    #[serde(deserialize_with = "nullable")]
    incident_updates: Vec<UpdateJson>,
}

#[derive(Deserialize, Default)]
#[serde(default)]
struct UpdateJson {
    #[serde(deserialize_with = "nullable")]
    body: String,
}

fn parse(body: &[u8]) -> Result<Summary, String> {
    let raw: SummaryJson =
        serde_json::from_slice(body).map_err(|e| format!("parsing status JSON: {e}"))?;

    let mut s = Summary {
        indicator: raw.status.indicator.trim().to_string(),
        description: raw.status.description.trim().to_string(),
        ..Default::default()
    };
    for inc in &raw.incidents {
        if is_resolved(&inc.status) {
            continue;
        }
        s.incidents.push(to_incident(inc, false));
    }
    for m in &raw.scheduled_maintenances {
        // Only maintenance that is actually running matters here. Upcoming
        // windows are not a disruption yet.
        if m.status != "in_progress" && m.status != "verifying" {
            continue;
        }
        s.incidents.push(to_incident(m, true));
    }
    for c in &raw.components {
        // Group rows only mirror the worst state of their children.
        if c.group {
            continue;
        }
        let comp = Component {
            name: c.name.trim().to_string(),
            status: c.status.trim().to_string(),
        };
        if comp.name.is_empty() || comp.operational() {
            continue;
        }
        s.affected.push(comp);
    }
    Ok(s)
}

fn is_resolved(status: &str) -> bool {
    matches!(
        status.trim().to_lowercase().as_str(),
        "resolved" | "postmortem" | "completed"
    )
}

fn to_incident(inc: &IncidentJson, maintenance: bool) -> Incident {
    Incident {
        id: inc.id.trim().to_string(),
        name: inc.name.trim().to_string(),
        status: inc.status.trim().to_string(),
        impact: inc.impact.trim().to_string(),
        url: inc.shortlink.trim().to_string(),
        // Statuspage lists updates newest-first.
        latest_update: inc
            .incident_updates
            .first()
            .map(|u| collapse_space(&u.body))
            .unwrap_or_default(),
        updated_at: DateTime::parse_from_rfc3339(inc.updated_at.trim()).ok(),
        maintenance,
    }
}

/// Flattens an update body to a single line, so that it fits a menu row or a
/// tooltip.
fn collapse_space(s: &str) -> String {
    s.split_whitespace().collect::<Vec<_>>().join(" ")
}

/// Turns a Statuspage machine value ("degraded_performance", "in_progress")
/// into a display label ("Degraded performance", "In progress").
pub fn human(s: &str) -> String {
    let s = s.replace('_', " ");
    let s = s.trim();
    let mut chars = s.chars();
    match chars.next() {
        None => String::new(),
        Some(first) => first.to_uppercase().chain(chars).collect(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A trimmed real-shaped summary.json with an ongoing incident, a resolved
    /// one (which must be ignored), an upcoming maintenance (also ignored) and a
    /// mix of component states.
    const OUTAGE_SAMPLE: &str = r#"{
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
}"#;

    const OPERATIONAL_SAMPLE: &str = r#"{
  "components": [{"name": "Claude API", "status": "operational", "group": false}],
  "incidents": [],
  "scheduled_maintenances": [],
  "status": {"indicator": "none", "description": "All Systems Operational"}
}"#;

    #[test]
    fn parse_operational() {
        let s = parse(OPERATIONAL_SAMPLE.as_bytes()).expect("parse");
        assert!(s.all_operational(), "indicator {:?}", s.indicator);
        assert!(s.incidents.is_empty() && s.affected.is_empty());
        assert_eq!(s.description, "All Systems Operational");
    }

    #[test]
    fn parse_outage() {
        let s = parse(OUTAGE_SAMPLE.as_bytes()).expect("parse");
        assert!(!s.all_operational(), "indicator {:?}", s.indicator);

        // The unresolved incident and the in-progress maintenance.
        assert_eq!(s.incidents.len(), 2);
        let inc = &s.incidents[0];
        assert_eq!(inc.name, "Elevated errors on the Claude API");
        assert_eq!(inc.status, "investigating");
        assert_eq!(inc.url, "https://stspg.io/inc1");
        // The newest update, with the whitespace collapsed.
        assert_eq!(
            inc.latest_update,
            "We are investigating elevated error rates."
        );
        assert!(inc.updated_at.is_some(), "expected updated_at to parse");
        let m = &s.incidents[1];
        assert!(m.maintenance && m.name == "Console maintenance", "{m:?}");

        // The two non-operational, non-group components.
        assert_eq!(s.affected.len(), 2, "{:?}", s.affected);
        assert_eq!(s.affected[0].name, "Claude API");
        assert_eq!(s.affected[1].name, "Models");
    }

    /// Go's decoder reads null as the zero value. One null field must not
    /// fail the whole document.
    #[test]
    fn parse_tolerates_nulls() {
        let body = r#"{
  "components": [{"name": "Claude API", "status": "major_outage", "group": null}],
  "incidents": [{"id": "i", "name": "Down", "status": "identified", "shortlink": null,
                 "updated_at": null, "incident_updates": null}],
  "scheduled_maintenances": null,
  "status": {"indicator": "major", "description": null}
}"#;
        let s = parse(body.as_bytes()).expect("parse");
        assert_eq!(s.description, "");
        assert_eq!(s.incidents.len(), 1);
        assert_eq!(s.incidents[0].url, "");
        assert_eq!(s.affected.len(), 1);
    }

    #[test]
    fn all_operational_treats_unknown_as_fine() {
        assert!(Summary::default().all_operational());
        for ind in ["minor", "major", "critical", "maintenance"] {
            let s = Summary {
                indicator: ind.into(),
                ..Default::default()
            };
            assert!(!s.all_operational(), "indicator {ind:?}");
        }
    }

    #[test]
    fn human_labels() {
        assert_eq!(human("degraded_performance"), "Degraded performance");
        assert_eq!(human("in_progress"), "In progress");
        assert_eq!(human(""), "");
    }

    /// Diagnostic: queries the live summary endpoint and prints what the menu
    /// would show. Opt-in, because it needs network access:
    ///
    ///     cargo test live_status -- --ignored --nocapture
    #[test]
    #[ignore]
    fn live_status() {
        let deadline = Instant::now() + Duration::from_secs(20);
        let s = fetch(DEFAULT_URL, deadline).expect("fetch");
        println!(
            "indicator = {:?} ({}), all_operational = {}",
            s.indicator,
            s.description,
            s.all_operational()
        );
        for inc in &s.incidents {
            println!(
                "incident: [{}] {} - {}",
                human(&inc.status),
                inc.name,
                inc.url
            );
        }
        for c in &s.affected {
            println!("affected: {} ({})", c.name, human(&c.status));
        }
        assert!(!s.description.is_empty(), "expected a status description");
    }
}
