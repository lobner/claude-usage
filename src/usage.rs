//! Gets the two headline meters shown on claude.ai/settings/usage - the 5-hour
//! "current session" window and the 7-day "all models" weekly window - directly
//! from Anthropic's OAuth usage endpoint, with Claude Code's stored OAuth token.
//!
//! It is READ-ONLY: it reads the token (from the macOS Keychain, with
//! ~/.claude/.credentials.json as the fallback) but never refreshes or writes
//! it. The token expires approximately every hour, and only the `claude` CLI
//! refreshes it. After it has expired, `fetch` returns `Error::TokenExpired`
//! until the next refresh.
//!
//! There is no officially documented API for this data. The endpoint is the
//! same internal one that the claude.ai usage page calls. See
//! <https://github.com/anthropics/claude-code/issues/13585>.

use std::fmt;
use std::io::Read;
use std::process::{Command, Stdio};
use std::time::{Duration, Instant, SystemTime};

use chrono::{DateTime, Utc};
use serde::Deserialize;
use wait_timeout::ChildExt;

use crate::duration;
use crate::http;
use crate::json::nullable;

const USAGE_URL: &str = "https://api.anthropic.com/api/oauth/usage";
const BETA_HEADER: &str = "oauth-2025-04-20"; // necessary, else the endpoint 401s
const KEYCHAIN_SERVICE: &str = "Claude Code-credentials";

const CLIENT_TIMEOUT: Duration = Duration::from_secs(20);
const BODY_LIMIT: u64 = 1 << 20; // 1 MiB

/// Why a fetch gave no meters.
#[derive(Debug, Clone, PartialEq)]
pub enum Error {
    /// The stored OAuth token is expired or was rejected. Callers keep the last
    /// known meters and tell the user to refresh it (run any Claude Code
    /// command). It is not a hard error.
    TokenExpired,
    /// The endpoint refused the request because it was called too recently.
    /// The measured limit is approximately one request every two minutes per
    /// token, so this is part of normal operation, not a fault: keep the last
    /// known meters and come back later.
    ///
    /// `retry_after` is how long the server asked us to wait, and zero when it
    /// said nothing useful. The endpoint usually sends "Retry-After: 0", so
    /// callers must apply their own spacing.
    RateLimited { retry_after: Duration },
    /// Any other failure, with a description for the menu.
    Other(String),
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Error::TokenExpired => f.write_str("oauth token expired"),
            Error::RateLimited { retry_after } if !retry_after.is_zero() => {
                write!(
                    f,
                    "rate limited, retry after {}",
                    duration::go_string(*retry_after)
                )
            }
            Error::RateLimited { .. } => f.write_str("rate limited"),
            Error::Other(msg) => f.write_str(msg),
        }
    }
}

/// A single usage window.
#[derive(Debug, Clone, Copy, Default, PartialEq)]
pub struct Meter {
    /// 0-100, rounded to match the usage page.
    pub percent: i64,
    /// When the window resets, if known.
    pub resets_at: Option<DateTime<Utc>>,
}

/// The pair of meters that the menu bar shows.
#[derive(Debug, Clone, Copy, Default, PartialEq)]
pub struct Usage {
    /// five_hour (the left number).
    pub session: Meter,
    /// seven_day (the right number).
    pub weekly: Meter,
}

/// Reads the stored OAuth token and queries the usage endpoint. All the work,
/// including the Keychain read, stops at `deadline`.
pub fn fetch(deadline: Instant) -> Result<Usage, Error> {
    let cred = load_credentials(deadline)?;
    // Do not hammer the endpoint with a known-dead token: if it has already
    // expired locally, report it without a doomed request. A live 401 is still
    // handled in fetch_with, in case expiresAt is missing or wrong.
    if let Some(expires_at) = cred.expires_at
        && SystemTime::now() > expires_at
    {
        return Err(Error::TokenExpired);
    }
    fetch_with(&cred.access_token, deadline)
}

fn fetch_with(token: &str, deadline: Instant) -> Result<Usage, Error> {
    let mut resp = http::agent()
        .get(USAGE_URL)
        .header("Authorization", &format!("Bearer {token}"))
        .header("anthropic-beta", BETA_HEADER)
        .config()
        .timeout_global(Some(http::timeout(CLIENT_TIMEOUT, deadline)))
        .build()
        .call()
        .map_err(|e| Error::Other(format!("usage request: {e}")))?;

    let code = resp.status().as_u16();
    let retry = resp
        .headers()
        .get("Retry-After")
        .and_then(|v| v.to_str().ok())
        .unwrap_or_default()
        .to_string();
    let body = http::read_limited(resp.body_mut(), BODY_LIMIT).unwrap_or_default();

    match code {
        401 | 403 => Err(Error::TokenExpired),
        429 => Err(Error::RateLimited {
            retry_after: retry_after(&retry),
        }),
        200 => parse(&body),
        _ => Err(Error::Other(format!(
            "usage endpoint HTTP {code}: {}",
            snippet(&body)
        ))),
    }
}

/// Reads the header in either of its forms - a delay in seconds, or an HTTP
/// date - and returns zero for anything else, including the "0" that this
/// endpoint usually sends.
fn retry_after(h: &str) -> Duration {
    let h = h.trim();
    if h.is_empty() {
        return Duration::ZERO;
    }
    if let Ok(secs) = h.parse::<i64>() {
        return if secs <= 0 {
            Duration::ZERO
        } else {
            Duration::from_secs(secs as u64)
        };
    }
    if let Ok(t) = httpdate::parse_http_date(h)
        && let Ok(d) = t.duration_since(SystemTime::now())
    {
        return d;
    }
    Duration::ZERO
}

#[derive(Deserialize, Default)]
#[serde(default)]
struct Window {
    utilization: Option<f64>,
    resets_at: Option<String>,
}

#[derive(Deserialize, Default)]
#[serde(default)]
struct ApiResponse {
    /// "error" on the error envelope, absent on success.
    #[serde(rename = "type", deserialize_with = "nullable")]
    kind: String,
    error: Option<ApiError>,
    five_hour: Option<Window>,
    seven_day: Option<Window>,
}

#[derive(Deserialize, Default)]
#[serde(default)]
struct ApiError {
    #[serde(rename = "type", deserialize_with = "nullable")]
    kind: String,
    #[serde(deserialize_with = "nullable")]
    message: String,
}

fn parse(body: &[u8]) -> Result<Usage, Error> {
    let r: ApiResponse = serde_json::from_slice(body)
        .map_err(|e| Error::Other(format!("parsing usage JSON: {e}")))?;
    if r.kind == "error" {
        return Err(match &r.error {
            Some(e) if e.kind == "authentication_error" => Error::TokenExpired,
            // Belt and braces: the status code catches the 429 first, but the
            // same envelope would otherwise read as an unexplained failure.
            Some(e) if e.kind == "rate_limit_error" => Error::RateLimited {
                retry_after: Duration::ZERO,
            },
            Some(e) if !e.message.is_empty() => Error::Other(e.message.clone()),
            _ => Error::Other("usage endpoint returned an error".into()),
        });
    }
    Ok(Usage {
        session: to_meter(r.five_hour.as_ref()),
        weekly: to_meter(r.seven_day.as_ref()),
    })
}

fn to_meter(w: Option<&Window>) -> Meter {
    let Some(w) = w else {
        return Meter::default();
    };
    let Some(utilization) = w.utilization else {
        return Meter::default();
    };
    Meter {
        // f64::round rounds halfway values away from zero, as Go's math.Round.
        percent: (utilization.round() as i64).clamp(0, 100),
        resets_at: w
            .resets_at
            .as_deref()
            .filter(|s| !s.is_empty())
            .and_then(|s| DateTime::parse_from_rfc3339(s).ok())
            .map(|t| t.with_timezone(&Utc)),
    }
}

fn snippet(b: &[u8]) -> String {
    let s = String::from_utf8_lossy(b);
    let s = s.trim();
    if s.len() <= 200 {
        return s.to_string();
    }
    let mut end = 200;
    while !s.is_char_boundary(end) {
        end -= 1;
    }
    format!("{}…", &s[..end])
}

// ---- credentials (read-only) ----

struct Credentials {
    access_token: String,
    expires_at: Option<SystemTime>,
}

/// The on-disk and Keychain shape. Only the necessary fields are read.
#[derive(Deserialize, Default)]
#[serde(default)]
struct CredBlob {
    #[serde(rename = "claudeAiOauth", deserialize_with = "nullable")]
    claude_ai_oauth: OauthBlob,
}

#[derive(Deserialize, Default)]
#[serde(default)]
struct OauthBlob {
    #[serde(rename = "accessToken", deserialize_with = "nullable")]
    access_token: String,
    /// Epoch milliseconds.
    #[serde(rename = "expiresAt", deserialize_with = "nullable")]
    expires_at: i64,
}

/// Reads Claude Code's OAuth credentials from the macOS Keychain, with
/// ~/.claude/.credentials.json as the fallback.
fn load_credentials(deadline: Instant) -> Result<Credentials, Error> {
    if let Some(blob) = keychain_read(deadline)
        && let Some(c) = parse_blob(&blob)
    {
        return Ok(c);
    }
    if let Some(home) = std::env::var_os("HOME").filter(|h| !h.is_empty()) {
        let path = std::path::Path::new(&home)
            .join(".claude")
            .join(".credentials.json");
        if let Ok(data) = std::fs::read(path)
            && let Some(c) = parse_blob(&data)
        {
            return Ok(c);
        }
    }
    Err(Error::Other(format!(
        "no Claude Code OAuth credentials found (Keychain item {KEYCHAIN_SERVICE:?} or ~/.claude/.credentials.json); log in with `claude` first"
    )))
}

/// Runs the system `security` tool. The absolute path is used because an app
/// started from Finder or at login gets a minimal PATH. The tool is stopped if
/// it is still running at `deadline`.
fn keychain_read(deadline: Instant) -> Option<Vec<u8>> {
    let mut child = Command::new("/usr/bin/security")
        .args(["find-generic-password", "-s", KEYCHAIN_SERVICE, "-w"])
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .spawn()
        .ok()?;
    let status = match child.wait_timeout(deadline.saturating_duration_since(Instant::now())) {
        Ok(Some(status)) => status,
        _ => {
            let _ = child.kill();
            let _ = child.wait();
            return None;
        }
    };
    if !status.success() {
        return None;
    }
    let mut out = Vec::new();
    child.stdout.take()?.read_to_end(&mut out).ok()?;
    Some(out)
}

fn parse_blob(data: &[u8]) -> Option<Credentials> {
    let b: CredBlob = serde_json::from_slice(data).ok()?;
    let oauth = b.claude_ai_oauth;
    if oauth.access_token.is_empty() {
        return None;
    }
    Some(Credentials {
        access_token: oauth.access_token,
        expires_at: (oauth.expires_at > 0)
            .then(|| SystemTime::UNIX_EPOCH + Duration::from_millis(oauth.expires_at as u64)),
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A trimmed real raw response. The codename windows are there to prove
    /// that only the stable keys are read.
    const RAW_SAMPLE: &str = r#"{
  "five_hour":        {"utilization": 7.0,  "resets_at": "2026-06-08T16:10:00.243011+00:00"},
  "seven_day":        {"utilization": 23.0, "resets_at": "2026-06-10T20:59:59.243039+00:00"},
  "seven_day_sonnet": {"utilization": 0.0,  "resets_at": null},
  "seven_day_opus":   null,
  "seven_day_omelette": {"utilization": 0.0, "resets_at": null},
  "extra_usage":      {"is_enabled": false}
}"#;

    #[test]
    fn parse_reads_stable_keys() {
        let u = parse(RAW_SAMPLE.as_bytes()).expect("parse");
        assert_eq!(u.session.percent, 7);
        assert_eq!(u.weekly.percent, 23);
        assert!(u.session.resets_at.is_some() && u.weekly.resets_at.is_some());
    }

    #[test]
    fn to_meter_rounds_and_clamps() {
        let w = |f: Option<f64>| Window {
            utilization: f,
            resets_at: None,
        };
        let cases = [
            (None, 0),
            (Some(w(None)), 0),
            (Some(w(Some(0.4))), 0),
            (Some(w(Some(2.5))), 3), // halfway rounds away from zero (math.Round)
            (Some(w(Some(99.6))), 100),
            (Some(w(Some(150.0))), 100), // clamp
        ];
        for (input, want) in cases {
            assert_eq!(to_meter(input.as_ref()).percent, want);
        }
    }

    /// Pins the contract main.rs relies on: a 429 must be recognisable as rate
    /// limiting, and must not read as a generic failure or an expired token.
    #[test]
    fn rate_limited_is_matchable() {
        let err = Error::RateLimited {
            retry_after: Duration::from_secs(90),
        };
        assert!(
            matches!(err, Error::RateLimited { retry_after } if retry_after == Duration::from_secs(90))
        );
        assert_ne!(err, Error::TokenExpired);
        assert!(err.to_string().contains("1m30s"), "{err}");
        let bare = Error::RateLimited {
            retry_after: Duration::ZERO,
        };
        assert_eq!(bare.to_string(), "rate limited");
    }

    #[test]
    fn retry_after_forms() {
        let cases = [
            ("", Duration::ZERO),
            ("0", Duration::ZERO), // what this endpoint actually sends
            ("-5", Duration::ZERO),
            ("30", Duration::from_secs(30)),
            (" 120 ", Duration::from_secs(120)),
            ("not a number", Duration::ZERO),
            ("Thu, 01 Jan 1970 00:00:01 GMT", Duration::ZERO), // a date in the past
        ];
        for (input, want) in cases {
            assert_eq!(retry_after(input), want, "retry_after({input:?})");
        }

        // A date in the future comes back as a positive wait.
        let future = httpdate::fmt_http_date(SystemTime::now() + Duration::from_secs(180));
        let got = retry_after(&future);
        assert!(
            got > Duration::from_secs(120) && got <= Duration::from_secs(180),
            "retry_after({future:?}) = {got:?}, want just under 3m"
        );
    }

    /// The belt-and-braces path: the same error envelope with a non-429 status.
    #[test]
    fn parse_rate_limit_envelope() {
        let body = r#"{"type":"error","error":{"type":"rate_limit_error","message":"Rate limited. Please try again later."}}"#;
        assert!(matches!(
            parse(body.as_bytes()),
            Err(Error::RateLimited { .. })
        ));
    }

    #[test]
    fn parse_auth_and_other_envelopes() {
        let auth = r#"{"type":"error","error":{"type":"authentication_error","message":"x"}}"#;
        assert_eq!(parse(auth.as_bytes()), Err(Error::TokenExpired));
        let other = r#"{"type":"error","error":{"type":"api_error","message":"Overloaded"}}"#;
        assert_eq!(
            parse(other.as_bytes()),
            Err(Error::Other("Overloaded".into()))
        );
        let bare = r#"{"type":"error"}"#;
        assert_eq!(
            parse(bare.as_bytes()),
            Err(Error::Other("usage endpoint returned an error".into()))
        );
    }

    #[test]
    fn parse_blob_shapes() {
        let c = parse_blob(br#"{"claudeAiOauth":{"accessToken":"t","expiresAt":1000}}"#).unwrap();
        assert_eq!(c.access_token, "t");
        assert_eq!(
            c.expires_at,
            Some(SystemTime::UNIX_EPOCH + Duration::from_secs(1))
        );
        // The Keychain prints the secret with a trailing newline.
        assert!(parse_blob(b"{\"claudeAiOauth\":{\"accessToken\":\"t\"}}\n").is_some());
        assert!(parse_blob(br#"{"claudeAiOauth":{"accessToken":""}}"#).is_none());
        assert!(parse_blob(br#"{"claudeAiOauth":null}"#).is_none());
        assert!(parse_blob(b"not json").is_none());
    }

    #[test]
    fn snippet_cuts_at_200_bytes() {
        let long = "x".repeat(300);
        assert_eq!(snippet(long.as_bytes()), format!("{}…", "x".repeat(200)));
        assert_eq!(snippet(b"  short  "), "short");
    }

    /// Diagnostic: queries the live usage endpoint with the stored OAuth token
    /// and prints the two meters. Opt-in, because it needs a valid Claude Code
    /// OAuth token:
    ///
    ///     cargo test live_fetch -- --ignored --nocapture
    #[test]
    #[ignore]
    fn live_fetch() {
        let u = fetch(Instant::now() + Duration::from_secs(25)).expect("fetch");
        println!(
            "session = {}% (resets {:?})",
            u.session.percent, u.session.resets_at
        );
        println!(
            "weekly  = {}% (resets {:?})",
            u.weekly.percent, u.weekly.resets_at
        );
    }
}
