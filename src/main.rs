//! Claude Usage is a macOS menu-bar app that shows your Claude subscription
//! usage as two percentages in the menu-bar title - "<session>% · <weekly>%",
//! where session is the current 5-hour window and weekly is the 7-day "all
//! models" limit - the same numbers as claude.ai/settings/usage. It reads the
//! usage endpoint directly with Claude Code's stored OAuth token, and posts a
//! Notification Centre banner when a meter crosses a threshold.
//!
//! It also polls status.claude.com: when the status page reports anything other
//! than "All Systems Operational", a red dot appears in front of the
//! percentages and the dropdown lists the ongoing incidents.
//!
//! The two sources are polled at different rates. The status page needs no
//! credentials and has no rate limit, so it is read every POLL_SECONDS. The
//! usage endpoint allows approximately one request every two minutes per token,
//! so it has its own USAGE_SECONDS cadence and backs off when it refuses.
//!
//! Environment overrides:
//!
//!     POLL_SECONDS   status-page interval in seconds (default 60, minimum 10)
//!     USAGE_SECONDS  usage-endpoint interval in seconds (default 150, minimum 120)
//!     ALERT_PERCENT  notify when a meter first reaches this % (default 80, 0=off)
//!     STATUS_URL     Statuspage summary document to poll (default status.claude.com)

mod duration;
mod http;
mod icon;
mod json;
mod login;
mod notify;
mod status;
mod tray;
mod usage;

use std::collections::HashSet;
use std::process::{Command, Stdio};
use std::sync::{LazyLock, Mutex, OnceLock};
use std::thread;
use std::time::{Duration, Instant};

use chrono::{DateTime, Local, TimeDelta, Utc};
use crossbeam_channel::{Receiver, Sender, bounded, select, tick};

use tray::MenuItem;

const DEFAULT_POLL_INTERVAL: Duration = Duration::from_secs(60);

const BLINK_INTERVAL: Duration = Duration::from_millis(700);

// The usage endpoint rate-limits at approximately one request per two minutes
// per token, and answers a 429 with "Retry-After: 0" - no budget headers,
// nothing to pace against. At the 60 s poll interval every other call came back
// 429, so the usage endpoint has its own, slower cadence, while the status page
// (no authentication, no limit) keeps to the poll interval.
const DEFAULT_USAGE_INTERVAL: Duration = Duration::from_secs(150);
const MIN_USAGE_INTERVAL: Duration = Duration::from_secs(120);
const MAX_USAGE_BACKOFF: Duration = Duration::from_secs(15 * 60);

const USAGE_PAGE_URL: &str = "https://claude.ai/settings/usage";
const STATUS_PAGE_URL: &str = "https://status.claude.com";

const APP_NAME: &str = "Claude Usage";
const REPO_URL: &str = "https://github.com/lobner/claude-usage";
const MAX_STATUS_ITEMS: usize = 5; // a fixed pool of incident rows (rows are never removed)
const STATUS_ROW_MAX_LEN: usize = 64; // characters for each incident row

// Build stamps, set by build/make-app.sh through the environment of `cargo
// build`. A plain `cargo build` or `cargo run` leaves them at these defaults,
// which is what "dev" in the About row means.
const VERSION: &str = match option_env!("CLAUDEUSAGE_VERSION") {
    Some(v) => v,
    None => "dev",
};
const COMMIT: &str = match option_env!("CLAUDEUSAGE_COMMIT") {
    Some(v) => v,
    None => "",
};
const BUILD_DATE: &str = match option_env!("CLAUDEUSAGE_BUILD_DATE") {
    Some(v) => v,
    None => "",
};

/// The environment overrides, read once at startup.
struct Config {
    poll_interval: Duration,
    usage_interval: Duration,
    alert_percent: i64,
    status_url: String,
}

static CONFIG: LazyLock<Config> = LazyLock::new(|| Config {
    poll_interval: poll_interval_from_env(),
    usage_interval: usage_interval_from_env(),
    alert_percent: alert_percent_from_env(),
    status_url: env_or("STATUS_URL", status::DEFAULT_URL),
});

/// The rows that change after startup.
struct Menu {
    session: MenuItem,
    session_reset: MenuItem,
    weekly: MenuItem,
    weekly_reset: MenuItem,
    status: MenuItem,
    status_items: Vec<MenuItem>,
    affected: MenuItem,
    last_check: MenuItem,
    login: MenuItem,
}

static MENU: OnceLock<Menu> = OnceLock::new();

fn menu() -> &'static Menu {
    MENU.get().expect("the menu is built in on_ready")
}

type Chan<T> = LazyLock<(Sender<T>, Receiver<T>)>;

static REFRESH_NOW: Chan<()> = LazyLock::new(|| bounded(1));

// The icon state, owned by icon_loop. check_status reports what the status page
// says. Opening the menu acknowledges an outage.
static ICON_UPDATES: Chan<IconUpdate> = LazyLock::new(|| bounded(1));
static ICON_ACK: Chan<()> = LazyLock::new(|| bounded(1));

// Shared between the poll thread (writer) and the click threads (readers).
static INCIDENT_URLS: Mutex<[String; MAX_STATUS_ITEMS]> =
    Mutex::new([const { String::new() }; MAX_STATUS_ITEMS]);

fn main() {
    tray::run(on_ready);
}

fn on_ready() {
    tray::set_title("…");
    tray::set_tooltip("Claude usage — checking…");

    let session = tray::add_item("Current session: …", "5-hour session window");
    session.disable();
    let session_reset = tray::add_item("", "");
    session_reset.disable();
    session_reset.hide();

    let weekly = tray::add_item("Weekly (all models): …", "7-day rolling window");
    weekly.disable();
    let weekly_reset = tray::add_item("", "");
    weekly_reset.disable();
    weekly_reset.hide();

    tray::add_separator();
    let status = tray::add_item("Service status: checking…", "Open status.claude.com");

    // A fixed pool of incident rows, shown, hidden and relabelled on each poll.
    let mut status_items = Vec::with_capacity(MAX_STATUS_ITEMS);
    for idx in 0..MAX_STATUS_ITEMS {
        let mi = tray::add_item("", "");
        mi.hide();
        on_click(&mi, move || open_url(&incident_url(idx)));
        status_items.push(mi);
    }
    let affected = tray::add_item("", "");
    affected.disable();
    affected.hide();

    tray::add_separator();
    let open_page = tray::add_item("Open usage page", "Open claude.ai/settings/usage");
    let refresh = tray::add_item("Refresh now", "Fetch usage immediately");
    let last_check = tray::add_item("", "");
    last_check.disable();
    tray::add_separator();

    let login = tray::add_checkbox(
        "Launch at Login",
        "Open Claude Usage automatically when you log in",
        login::enabled(),
    );
    if login::bundle_path().is_none() || !login::supported() {
        login.hide(); // nothing to register: not a bundle, or macOS 12 or earlier
    }
    tray::add_separator();

    let about = tray::add_item(&about_title(), &about_tooltip());
    let quit = tray::add_item("Quit", "Quit Claude Usage");

    on_click(&open_page, || open_url(USAGE_PAGE_URL));
    on_click(&status, || open_url(STATUS_PAGE_URL));
    on_click(&refresh, trigger_refresh);
    on_click(&login, toggle_launch_at_login);
    on_click(&about, || open_url(&release_notes_url()));
    thread::spawn(move || {
        let _ = quit.clicked().recv();
        tray::quit();
    });

    // Opening the menu means the user has seen the outage: stop the blinking.
    tray::on_menu_will_open(|| {
        let _ = ICON_ACK.0.try_send(()); // or an acknowledgement is already queued
    });

    let _ = MENU.set(Menu {
        session,
        session_reset,
        weekly,
        weekly_reset,
        status,
        status_items,
        affected,
        last_check,
        login,
    });

    thread::spawn(icon_loop);
    thread::spawn(|| Poller::default().run());
    thread::spawn(offer_launch_at_login);
}

/// Runs `f` on its own thread for each click on `item`, one click at a time.
fn on_click(item: &MenuItem, f: impl Fn() + Send + 'static) {
    let clicks = item.clicked().clone();
    thread::spawn(move || {
        for () in clicks.iter() {
            f();
        }
    });
}

/// Names the app and the build in one line, so the version is visible without
/// a click.
fn about_title() -> String {
    format!("About {APP_NAME} {VERSION}")
}

/// Carries what is necessary to identify a build in a bug report. The
/// architecture is there because the released archives are for Apple silicon
/// only, and the commit because these builds are unsigned - "which build is
/// that?" otherwise has no answer.
fn about_tooltip() -> String {
    let mut parts = Vec::with_capacity(4);
    if !COMMIT.is_empty() {
        parts.push(format!("commit {COMMIT}"));
    }
    if !BUILD_DATE.is_empty() {
        parts.push(format!("built {BUILD_DATE}"));
    }
    parts.push(platform());
    parts.join(" · ") + " — click for the release notes"
}

/// The platform in Go's GOOS/GOARCH words, which the About tooltip has always
/// used.
fn platform() -> String {
    let os = match std::env::consts::OS {
        "macos" => "darwin",
        other => other,
    };
    let arch = match std::env::consts::ARCH {
        "aarch64" => "arm64",
        "x86_64" => "amd64",
        other => other,
    };
    format!("{os}/{arch}")
}

/// Points at the notes of this exact version when the build came from a clean
/// tag, and at the release list otherwise, since there is nothing to link a
/// dev build to.
fn release_notes_url() -> String {
    release_notes_url_for(VERSION)
}

fn release_notes_url_for(version: &str) -> String {
    if version.starts_with('v') && !version.contains(['-', '+']) {
        format!("{REPO_URL}/releases/tag/{version}")
    } else {
        format!("{REPO_URL}/releases")
    }
}

/// Asks - once - whether the app should open at login, and registers it as a
/// login item if so. It stays quiet when there is nothing to offer: when the
/// user has already answered either way, when the binary is not running from
/// its .app bundle, or on a macOS without SMAppService.
fn offer_launch_at_login() {
    if login::bundle_path().is_none() || !login::supported() {
        return;
    }
    let m = menu();

    // Already answered: do not ask again, but make the system match what we
    // recorded - see login::reconcile for why it cannot be read back instead.
    if login::answered() {
        if login::answer() {
            if login::reconcile() {
                m.login.check();
            } else {
                m.login.uncheck();
            }
        }
        return;
    }

    let Ok(yes) = notify::confirm(
        "Launch Claude Usage at login?",
        "Claude Usage can start automatically when you log in, so your session and weekly meters are always in the menu bar.",
        "Launch at Login",
        "Not Now",
    ) else {
        return; // the dialog itself failed; try again next launch, do not guess
    };
    if !yes {
        let _ = login::record_answer(false);
        return;
    }

    if let Err(e) = login::enable() {
        // Do not record the answer, so the offer is made again next launch.
        let _ = notify::banner("Claude Usage", &format!("Could not open at login: {e}"));
        return;
    }
    let _ = login::record_answer(true);
    m.login.check();
    warn_if_needs_approval();
}

/// Flips the Launch at Login checkbox. The checkbox changes only after the
/// change has gone through, so a failure leaves the menu showing the truth.
fn toggle_launch_at_login() {
    let m = menu();
    if m.login.checked() {
        if let Err(e) = login::disable() {
            let _ = notify::banner(
                "Claude Usage",
                &format!("Could not stop opening at login: {e}"),
            );
            return;
        }
        let _ = login::record_answer(false);
        m.login.uncheck();
        return;
    }

    if let Err(e) = login::enable() {
        let _ = notify::banner("Claude Usage", &format!("Could not open at login: {e}"));
        return;
    }
    let _ = login::record_answer(true);
    m.login.check();
    warn_if_needs_approval();
}

/// Covers the case where the user has switched the login item off in System
/// Settings before: the registration succeeds, but macOS holds it back until
/// they switch it on there.
fn warn_if_needs_approval() {
    if login::needs_approval() {
        let _ = notify::banner(
            "Claude Usage",
            "Switch Claude Usage on under System Settings → General → Login Items to finish enabling it.",
        );
    }
}

/// The state of the poll thread. Nothing else reads or writes it.
#[derive(Default)]
struct Poller {
    first_run_done: bool,
    alerted_session: bool,
    alerted_weekly: bool,
    have_status: bool,
    last_incidents: HashSet<String>,

    // The pacing of the usage endpoint. next_usage is when the endpoint may be
    // called again. The backoff grows while it keeps saying no.
    next_usage: Option<Instant>,
    usage_backoff: Duration,
}

impl Poller {
    fn run(mut self) {
        self.check(); // the first check is immediate
        let ticker = tick(CONFIG.poll_interval);
        loop {
            select! {
                recv(ticker) -> _ => self.check(),
                recv(REFRESH_NOW.1) -> _ => {
                    // "Refresh now" means now: drop the pacing so that the usage
                    // endpoint is called. If it refuses, the menu says so.
                    self.next_usage = None;
                    self.usage_backoff = Duration::ZERO;
                    self.check();
                }
            }
        }
    }

    fn check(&mut self) {
        let deadline = Instant::now() + Duration::from_secs(25);
        let m = menu();

        // The status page needs no credentials and has no rate limit, so check
        // it first and on every poll: the dot must keep working while the usage
        // endpoint is not available for any reason.
        self.check_status(deadline);

        // Respect the pacing that the endpoint forces on us, instead of calling
        // it every poll and collecting 429s.
        if self.next_usage.is_some_and(|n| Instant::now() < n) {
            return;
        }

        let result = usage::fetch(deadline);
        let now = Instant::now();
        let wall = Local::now();
        let stamp = wall.format("%H:%M:%S").to_string();

        let u = match result {
            Err(usage::Error::RateLimited { retry_after }) => {
                // Expected, not a fault: keep the numbers on screen and space out.
                let wait = self.back_off_usage(retry_after);
                self.next_usage = Some(now + wait);
                let retry_at = wall + wait;
                m.last_check.set_title(&format!(
                    "Rate limited; retrying {}",
                    retry_at.format("%H:%M:%S")
                ));
                m.last_check.set_tooltip(&format!(
                    "The usage endpoint allows about one request every two minutes. Backing off {}.",
                    duration::go_string(duration::round_to_second(wait))
                ));
                return;
            }
            Err(usage::Error::TokenExpired) => {
                // Keep the last known numbers. Only flag that the token needs a refresh.
                tray::set_title("⚠");
                m.session.set_title("⚠ OAuth token expired");
                m.weekly.set_title("Run a Claude Code command to refresh");
                m.session_reset.hide();
                m.weekly_reset.hide();
                tray::set_tooltip("Claude usage — token expired; refresh via Claude Code");
                m.last_check.set_title(&format!("Token expired {stamp}"));
                m.last_check.set_tooltip("");
                self.next_usage = Some(now + CONFIG.usage_interval);
                return;
            }
            Err(err) => {
                // Say what went wrong: a bare "failed" looks the same as the
                // rate limiting above, which made this hard to diagnose.
                tray::set_tooltip("Claude usage — update failed");
                m.last_check.set_title(&format!(
                    "Last check failed {stamp} — {}",
                    truncate(&reason(&err.to_string()), 48)
                ));
                m.last_check.set_tooltip(&err.to_string());
                self.next_usage = Some(now + CONFIG.usage_interval);
                return;
            }
            Ok(u) => u,
        };

        self.usage_backoff = Duration::ZERO;
        self.next_usage = Some(now + CONFIG.usage_interval);

        update_ui(&u);
        self.notify_thresholds(&u);
        m.last_check.set_title(&format!("Last checked {stamp}"));
        m.last_check.set_tooltip("");
    }

    /// Doubles the wait each time the endpoint refuses, from one usage interval
    /// up to MAX_USAGE_BACKOFF, and uses the server's Retry-After instead when
    /// that asks for longer.
    fn back_off_usage(&mut self, retry_after: Duration) -> Duration {
        if self.usage_backoff.is_zero() {
            self.usage_backoff = CONFIG.usage_interval;
        } else if self.usage_backoff < MAX_USAGE_BACKOFF {
            self.usage_backoff *= 2;
        }
        if self.usage_backoff > MAX_USAGE_BACKOFF {
            self.usage_backoff = MAX_USAGE_BACKOFF;
        }
        if retry_after > self.usage_backoff {
            return retry_after;
        }
        self.usage_backoff
    }

    /// Sends a banner the first time a meter reaches the alert percentage, and
    /// arms again after it drops back below (for example after a reset). The
    /// first poll only sets a baseline, so a launch while already high does not
    /// spam.
    fn notify_thresholds(&mut self, u: &usage::Usage) {
        let alert = CONFIG.alert_percent;
        if alert <= 0 {
            return;
        }
        let s_high = u.session.percent >= alert;
        let w_high = u.weekly.percent >= alert;

        if self.first_run_done {
            if s_high && !self.alerted_session {
                let _ = notify::banner(
                    "Claude session usage high",
                    &format!("Current session at {}%", u.session.percent),
                );
            }
            if w_high && !self.alerted_weekly {
                let _ = notify::banner(
                    "Claude weekly usage high",
                    &format!("Weekly (all models) at {}%", u.weekly.percent),
                );
            }
        }
        self.alerted_session = s_high;
        self.alerted_weekly = w_high;
        self.first_run_done = true;
    }

    /// Polls status.claude.com and refreshes the dot and the incident rows. A
    /// failed poll keeps what was last known: a flaky network must not silently
    /// claim that everything is fine.
    fn check_status(&mut self, deadline: Instant) {
        let s = match status::fetch(&CONFIG.status_url, deadline) {
            Ok(s) => s,
            Err(_) => {
                if !self.have_status {
                    menu().status.set_title("Service status unavailable");
                }
                return;
            }
        };
        self.have_status = true;
        update_status_ui(&s);
        // Outside update_status_ui on purpose: that holds INCIDENT_URLS, and this
        // send can block, which would stall the click handlers that need it.
        self.report_status_to_icon(&s);
    }

    /// Tells icon_loop what the status page says: whether anything is wrong, and
    /// whether any of it is new since the last poll. An outage with no listed
    /// incidents still counts as ongoing - the indicator alone is enough.
    fn report_status_to_icon(&mut self, s: &status::Summary) {
        let cur: HashSet<String> = s.incidents.iter().map(|i| i.id.clone()).collect();
        let is_new = cur.iter().any(|id| !self.last_incidents.contains(id));
        self.last_incidents = cur;
        let _ = ICON_UPDATES.0.send(IconUpdate {
            ongoing: !s.all_operational(),
            is_new,
        });
    }
}

/// Reduces an error to something that fits in a menu row: the first line,
/// without the multi-line JSON body that some failures carry.
fn reason(s: &str) -> String {
    let first = s.split(['\n', '\r']).next().unwrap_or_default();
    let first = first.trim();
    first.strip_suffix(':').unwrap_or(first).trim().to_string()
}

fn update_ui(u: &usage::Usage) {
    let m = menu();
    let (s, w) = (u.session.percent, u.weekly.percent);
    tray::set_title(&format!("{s}% · {w}%"));
    tray::set_tooltip(&format!("Claude usage — session {s}% · weekly {w}%"));

    m.session.set_title(&format!("Current session: {s}%"));
    m.weekly.set_title(&format!("Weekly (all models): {w}%"));
    set_reset(&m.session_reset, &u.session);
    set_reset(&m.weekly_reset, &u.weekly);
}

fn set_reset(mi: &MenuItem, meter: &usage::Meter) {
    let Some(t) = meter.resets_at else {
        mi.hide();
        return;
    };
    mi.set_title(&format!("    resets {}", humanize_reset(t, Utc::now())));
    mi.show();
}

/// Formats a reset time as a short relative string for windows that reset
/// within a day ("in 1h 59m"), or as an absolute weekday and time otherwise
/// ("on Wed 22:59").
fn humanize_reset(t: DateTime<Utc>, now: DateTime<Utc>) -> String {
    let d = t - now;
    if d <= TimeDelta::zero() {
        "now".into()
    } else if d < TimeDelta::hours(1) {
        format!("in {}m", d.num_minutes())
    } else if d < TimeDelta::hours(24) {
        format!("in {}h {}m", d.num_hours(), d.num_minutes() % 60)
    } else {
        format!("on {}", t.with_timezone(&Local).format("%a %H:%M"))
    }
}

fn update_status_ui(s: &status::Summary) {
    let m = menu();
    let mut urls = INCIDENT_URLS.lock().unwrap();
    for u in urls.iter_mut() {
        u.clear();
    }

    if s.all_operational() {
        m.status.set_title("✓  All services operational");
        m.status
            .set_tooltip("status.claude.com reports all services operational");
        for mi in &m.status_items {
            mi.hide();
        }
        m.affected.hide();
        return;
    }

    let desc = if s.description.is_empty() {
        format!("{} service disruption", status::human(&s.indicator))
    } else {
        s.description.clone()
    };
    m.status.set_title(&format!("●  {desc}"));
    m.status
        .set_tooltip("status.claude.com — click to open the status page");

    if s.incidents.len() > MAX_STATUS_ITEMS {
        m.status.set_title(&format!(
            "●  {desc}  (showing {MAX_STATUS_ITEMS} of {})",
            s.incidents.len()
        ));
    }

    for (i, mi) in m.status_items.iter().enumerate() {
        let Some(inc) = s.incidents.get(i) else {
            mi.hide();
            continue;
        };
        let label = if inc.status.is_empty() {
            inc.name.clone()
        } else {
            format!("{} — {}", status::human(&inc.status), inc.name)
        };
        mi.set_title(&format!("    {}", truncate(&label, STATUS_ROW_MAX_LEN)));
        mi.set_tooltip(&incident_tooltip(inc));
        urls[i] = inc.url.clone();
        mi.show();
    }

    if s.affected.is_empty() {
        m.affected.hide();
        return;
    }
    let list = s
        .affected
        .iter()
        .map(|c| format!("{} ({})", c.name, status::human(&c.status)))
        .collect::<Vec<_>>()
        .join(", ");
    m.affected.set_title(&format!(
        "    Affected: {}",
        truncate(&list, STATUS_ROW_MAX_LEN)
    ));
    m.affected.set_tooltip(&format!("Affected: {list}"));
    m.affected.show();
}

fn incident_tooltip(inc: &status::Incident) -> String {
    let mut tip = inc.name.clone();
    if !inc.latest_update.is_empty() {
        tip += " — ";
        tip += &inc.latest_update;
    }
    truncate(&tip, 300)
}

/// What the latest status poll saw.
struct IconUpdate {
    ongoing: bool,
    is_new: bool,
}

#[derive(Clone, Copy, PartialEq)]
enum IconState {
    /// All operational: no icon at all, text only.
    Ok,
    /// An outage the user has not seen yet: a blinking red dot.
    Alert,
    /// Seen by the user: a steady red dot.
    Ack,
}

/// Owns the menu-bar icon. It is the only writer, so the blink can never race a
/// state change and leave the wrong icon on screen. The dot blinks until the
/// user opens the menu, then stays solid for as long as the outage lasts.
fn icon_loop() {
    let mut st = IconState::Ok;
    let mut red = false;

    let set_red = |red: &mut bool, on: bool| {
        *red = on;
        if on {
            tray::set_icon(Some(icon::red_dot_png()));
        } else {
            // The blink is off, the outage is not over: a transparent image of
            // the same size holds the icon slot so the percentages do not move.
            tray::set_icon(Some(icon::blank_png()));
        }
    };
    let apply = |st: &mut IconState, red: &mut bool, next: IconState| {
        *st = next;
        match next {
            IconState::Ok => {
                // All clear: remove the icon, so it is text only again.
                *red = false;
                tray::set_icon(None);
            }
            IconState::Alert => set_red(red, true), // start lit, so it is seen immediately
            IconState::Ack => {
                *red = true;
                tray::set_icon(Some(icon::red_dot_png()));
            }
        }
    };

    let ticker = tick(BLINK_INTERVAL);
    loop {
        select! {
            recv(ICON_UPDATES.1) -> u => {
                let Ok(u) = u else { return };
                if !u.ongoing {
                    if st != IconState::Ok {
                        apply(&mut st, &mut red, IconState::Ok);
                    }
                } else if u.is_new || st == IconState::Ok {
                    // A new incident starts the blink again, even if an earlier
                    // one has already been acknowledged.
                    apply(&mut st, &mut red, IconState::Alert);
                }
            }
            recv(ICON_ACK.1) -> _ => {
                if st == IconState::Alert {
                    apply(&mut st, &mut red, IconState::Ack);
                }
            }
            recv(ticker) -> _ => {
                if st == IconState::Alert {
                    let on = !red;
                    set_red(&mut red, on);
                }
            }
        }
    }
}

/// Cuts `s` to `n` characters, with "…" as the last one when it is cut.
fn truncate(s: &str, n: usize) -> String {
    if s.chars().count() <= n {
        return s.to_string();
    }
    if n < 1 {
        return String::new();
    }
    s.chars().take(n - 1).chain(std::iter::once('…')).collect()
}

fn incident_url(i: usize) -> String {
    INCIDENT_URLS
        .lock()
        .unwrap()
        .get(i)
        .cloned()
        .unwrap_or_default()
}

fn trigger_refresh() {
    let _ = REFRESH_NOW.0.try_send(()); // or a refresh is already queued
}

/// Opens `u` in the default browser, without waiting for it.
fn open_url(u: &str) {
    if u.is_empty() {
        return;
    }
    let child = Command::new("/usr/bin/open")
        .arg(u)
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn();
    if let Ok(mut child) = child {
        thread::spawn(move || child.wait());
    }
}

/// Parses a whole number as Go's strconv.Atoi does: an optional sign, then
/// digits only.
fn atoi(v: &str) -> Option<i64> {
    v.parse().ok()
}

/// Reads USAGE_SECONDS, and refuses anything below MIN_USAGE_INTERVAL: going
/// faster than the endpoint's own limit only gives 429s, so a smaller value
/// would make the meter worse, not fresher.
fn usage_interval_from_env() -> Duration {
    usage_interval_from(std::env::var("USAGE_SECONDS").ok().as_deref())
}

fn usage_interval_from(v: Option<&str>) -> Duration {
    match v.filter(|v| !v.is_empty()).and_then(atoi) {
        Some(n) if n >= MIN_USAGE_INTERVAL.as_secs() as i64 => Duration::from_secs(n as u64),
        _ => DEFAULT_USAGE_INTERVAL,
    }
}

fn poll_interval_from_env() -> Duration {
    poll_interval_from(std::env::var("POLL_SECONDS").ok().as_deref())
}

fn poll_interval_from(v: Option<&str>) -> Duration {
    match v.filter(|v| !v.is_empty()).and_then(atoi) {
        Some(n) if n >= 10 => Duration::from_secs(n as u64),
        _ => DEFAULT_POLL_INTERVAL,
    }
}

fn env_or(key: &str, def: &str) -> String {
    std::env::var(key)
        .ok()
        .filter(|v| !v.is_empty())
        .unwrap_or_else(|| def.to_string())
}

fn alert_percent_from_env() -> i64 {
    alert_percent_from(std::env::var("ALERT_PERCENT").ok().as_deref())
}

fn alert_percent_from(v: Option<&str>) -> i64 {
    match v.filter(|v| !v.is_empty()).and_then(atoi) {
        Some(n) if (0..=100).contains(&n) => n,
        _ => 80,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn truncate_counts_characters() {
        assert_eq!(truncate("short", 10), "short");
        assert_eq!(truncate("exactly10!", 10), "exactly10!");
        assert_eq!(truncate("much too long", 5), "much…");
        assert_eq!(truncate("æøå æøå", 4), "æøå…");
        assert_eq!(truncate("x", 0), "");
    }

    #[test]
    fn reason_takes_the_first_line() {
        assert_eq!(
            reason("usage endpoint HTTP 500: {\n \"a\": 1}"),
            "usage endpoint HTTP 500: {"
        );
        assert_eq!(reason("  parsing failed:  \nbody"), "parsing failed");
        assert_eq!(reason("plain"), "plain");
    }

    #[test]
    fn release_notes_url_only_for_clean_tags() {
        assert_eq!(
            release_notes_url_for("v1.2.0"),
            "https://github.com/lobner/claude-usage/releases/tag/v1.2.0"
        );
        for v in [
            "dev",
            "v1.2.0-2-gabc1234",
            "v1.2.0-dirty",
            "v1.2.0+meta",
            "abc1234",
        ] {
            assert_eq!(
                release_notes_url_for(v),
                "https://github.com/lobner/claude-usage/releases",
                "{v}"
            );
        }
    }

    #[test]
    fn env_parsing() {
        assert_eq!(poll_interval_from(None), DEFAULT_POLL_INTERVAL);
        assert_eq!(poll_interval_from(Some("9")), DEFAULT_POLL_INTERVAL);
        assert_eq!(poll_interval_from(Some("10")), Duration::from_secs(10));
        assert_eq!(poll_interval_from(Some("abc")), DEFAULT_POLL_INTERVAL);

        assert_eq!(usage_interval_from(Some("119")), DEFAULT_USAGE_INTERVAL);
        assert_eq!(usage_interval_from(Some("120")), Duration::from_secs(120));
        assert_eq!(usage_interval_from(Some("-500")), DEFAULT_USAGE_INTERVAL);

        assert_eq!(alert_percent_from(None), 80);
        assert_eq!(alert_percent_from(Some("0")), 0);
        assert_eq!(alert_percent_from(Some("100")), 100);
        assert_eq!(alert_percent_from(Some("101")), 80);
        assert_eq!(alert_percent_from(Some("-1")), 80);
    }

    #[test]
    fn back_off_doubles_up_to_the_cap() {
        let mut p = Poller::default();
        let base = CONFIG.usage_interval;
        assert_eq!(p.back_off_usage(Duration::ZERO), base);
        assert_eq!(p.back_off_usage(Duration::ZERO), base * 2);
        assert_eq!(
            p.back_off_usage(Duration::from_secs(3600)),
            Duration::from_secs(3600),
            "a longer Retry-After wins"
        );
        for _ in 0..10 {
            p.back_off_usage(Duration::ZERO);
        }
        assert_eq!(p.back_off_usage(Duration::ZERO), MAX_USAGE_BACKOFF);
    }

    #[test]
    fn humanize_reset_forms() {
        let now = Utc::now();
        let mins = |m: i64| now + chrono::TimeDelta::minutes(m) + chrono::TimeDelta::seconds(30);
        assert_eq!(humanize_reset(now, now), "now");
        assert_eq!(
            humanize_reset(now - chrono::TimeDelta::minutes(1), now),
            "now"
        );
        assert_eq!(humanize_reset(mins(5), now), "in 5m");
        assert_eq!(humanize_reset(mins(119), now), "in 1h 59m");
        let far = now + chrono::TimeDelta::days(3);
        assert_eq!(
            humanize_reset(far, now),
            format!("on {}", far.with_timezone(&Local).format("%a %H:%M"))
        );
    }

    #[test]
    #[cfg(all(target_os = "macos", target_arch = "aarch64"))]
    fn platform_uses_go_names() {
        assert_eq!(platform(), "darwin/arm64");
    }
}
