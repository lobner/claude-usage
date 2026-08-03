// Command claudeusage is a macOS menu-bar app that shows your Claude subscription
// usage as two percentages in the menu-bar title — "<session>% · <weekly>%",
// where session is the current 5-hour window and weekly is the 7-day "all models"
// limit — the same numbers as claude.ai/settings/usage. It reads the usage
// endpoint directly with Claude Code's stored OAuth token and posts a
// Notification Centre banner when a meter crosses a threshold.
//
// It also polls status.claude.com: whenever the status page reports anything
// other than "All Systems Operational", a red dot appears in front of the
// percentages and the dropdown lists the ongoing incidents.
//
// The two sources are polled at different rates. The status page needs no
// credentials and is not rate limited, so it is read every POLL_SECONDS. The
// usage endpoint allows roughly one request every two minutes per token, so it
// gets its own USAGE_SECONDS cadence and backs off when refused.
//
// Environment overrides:
//
//	POLL_SECONDS   status-page interval in seconds (default 60, minimum 10)
//	USAGE_SECONDS  usage-endpoint interval in seconds (default 150, minimum 120)
//	ALERT_PERCENT  notify when a meter first reaches this % (default 80, 0=off)
//	STATUS_URL     Statuspage summary document to poll (default status.claude.com)
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"fyne.io/systray"

	"claudeusage/internal/icon"
	"claudeusage/internal/login"
	"claudeusage/internal/notify"
	"claudeusage/internal/status"
	"claudeusage/internal/usage"
)

const (
	defaultPollInterval = 60 * time.Second

	// The usage endpoint rate-limits at roughly one request per two minutes per
	// token, and answers a 429 with "Retry-After: 0" — no budget headers, nothing
	// to pace against. At the 60 s poll interval every other call came back 429,
	// so the usage endpoint gets its own, slower cadence while the status page
	// (unauthenticated, unlimited) keeps to pollInterval.
	blinkInterval = 700 * time.Millisecond

	defaultUsageInterval = 150 * time.Second
	minUsageInterval     = 120 * time.Second
	maxUsageBackoff      = 15 * time.Minute

	usagePageURL  = "https://claude.ai/settings/usage"
	statusPageURL = "https://status.claude.com"

	appName         = "Claude Usage"
	repoURL         = "https://github.com/lobner/claude-usage"
	maxStatusItems  = 5  // fixed pool of incident rows (systray can't remove items)
	statusRowMaxLen = 64 // characters per incident row
)

// Build stamps, set by build/make-app.sh with -ldflags. A plain `go build` or
// `go run .` leaves them at these defaults, which is what "dev" in the About row
// means.
var (
	version   = "dev"
	commit    = ""
	buildDate = ""
)

// noIcon is deliberately undecodable image data: systray has no "remove the
// icon" call, but AppKit turns data it cannot decode into a nil NSImage, which
// drops the image from the status item (leaving the title text alone).
var noIcon = []byte{0}

var (
	pollInterval  = pollIntervalFromEnv()
	usageInterval = usageIntervalFromEnv()
	alertPercent  = alertPercentFromEnv()
	statusURL     = envOr("STATUS_URL", status.DefaultURL)

	mSession      *systray.MenuItem
	mSessionReset *systray.MenuItem
	mWeekly       *systray.MenuItem
	mWeeklyReset  *systray.MenuItem
	mStatus       *systray.MenuItem
	mStatusItems  []*systray.MenuItem
	mAffected     *systray.MenuItem
	mLastCheck    *systray.MenuItem
	mLogin        *systray.MenuItem

	refreshNow = make(chan struct{}, 1)

	// Icon state, owned by iconLoop. checkStatus reports what the status page
	// says; opening the menu acknowledges an outage.
	iconUpdates = make(chan iconUpdate, 1)
	iconAck     = make(chan struct{}, 1)

	// Poll-goroutine-only state.
	last           usage.Usage
	haveData       bool
	firstRun       = true
	alertedSession bool
	alertedWeekly  bool
	haveStatus     bool
	lastIncidents  = map[string]bool{}

	// Usage-endpoint pacing, also poll-goroutine-only. nextUsage is when the
	// endpoint may be called again; backoff grows while it keeps saying no.
	nextUsage    time.Time
	usageBackoff time.Duration

	// Shared between the poll goroutine (writer) and click goroutines (readers).
	mu           sync.Mutex
	incidentURLs = make([]string, maxStatusItems)
)

func main() {
	systray.Run(onReady, onExit)
}

func onReady() {
	systray.SetTitle("…")
	systray.SetTooltip("Claude usage — checking…")

	mSession = systray.AddMenuItem("Current session: …", "5-hour session window")
	mSession.Disable()
	mSessionReset = systray.AddMenuItem("", "")
	mSessionReset.Disable()
	mSessionReset.Hide()

	mWeekly = systray.AddMenuItem("Weekly (all models): …", "7-day rolling window")
	mWeekly.Disable()
	mWeeklyReset = systray.AddMenuItem("", "")
	mWeeklyReset.Disable()
	mWeeklyReset.Hide()

	systray.AddSeparator()
	mStatus = systray.AddMenuItem("Service status: checking…", "Open status.claude.com")

	// Pre-allocate a fixed pool of incident rows, shown/hidden and relabelled on
	// each poll.
	for i := 0; i < maxStatusItems; i++ {
		mi := systray.AddMenuItem("", "")
		mi.Hide()
		mStatusItems = append(mStatusItems, mi)
		go func(idx int) {
			for range mi.ClickedCh {
				openURL(incidentURL(idx))
			}
		}(i)
	}
	mAffected = systray.AddMenuItem("", "")
	mAffected.Disable()
	mAffected.Hide()

	systray.AddSeparator()
	mOpenPage := systray.AddMenuItem("Open usage page", "Open claude.ai/settings/usage")
	mRefresh := systray.AddMenuItem("Refresh now", "Fetch usage immediately")
	mLastCheck = systray.AddMenuItem("", "")
	mLastCheck.Disable()
	systray.AddSeparator()

	mLogin = systray.AddMenuItemCheckbox("Launch at Login",
		"Open Claude Usage automatically when you log in", login.Enabled())
	if login.BundlePath() == "" || !login.Supported() {
		mLogin.Hide() // nothing to register: not a bundle, or macOS 12 or earlier
	}
	systray.AddSeparator()

	mAbout := systray.AddMenuItem(aboutTitle(), aboutTooltip())
	mQuit := systray.AddMenuItem("Quit", "Quit Claude Usage")

	go func() {
		for range mOpenPage.ClickedCh {
			openURL(usagePageURL)
		}
	}()
	go func() {
		for range mStatus.ClickedCh {
			openURL(statusPageURL)
		}
	}()
	go func() {
		for range mRefresh.ClickedCh {
			triggerRefresh()
		}
	}()
	go func() {
		for range mLogin.ClickedCh {
			toggleLaunchAtLogin()
		}
	}()
	go func() {
		for range mAbout.ClickedCh {
			openURL(releaseNotesURL())
		}
	}()
	go func() {
		<-mQuit.ClickedCh
		systray.Quit()
	}()

	// Opening the menu means the user has seen the outage: stop the blinking.
	go func() {
		for range systray.TrayOpenedCh {
			select {
			case iconAck <- struct{}{}:
			default: // an acknowledgement is already queued
			}
		}
	}()

	go iconLoop()
	go pollLoop()
	go offerLaunchAtLogin()
}

func onExit() {}

// aboutTitle names the app and the build in one line, so the version is readable
// without clicking anything.
func aboutTitle() string { return "About " + appName + " " + version }

// aboutTooltip carries what is needed to identify a build in a bug report. The
// architecture is included because the released archives are Apple silicon only,
// and the commit because these builds are unsigned — "which build is that?" is
// otherwise unanswerable.
func aboutTooltip() string {
	parts := make([]string, 0, 4)
	if commit != "" {
		parts = append(parts, "commit "+commit)
	}
	if buildDate != "" {
		parts = append(parts, "built "+buildDate)
	}
	parts = append(parts, runtime.GOOS+"/"+runtime.GOARCH)
	return strings.Join(parts, " · ") + " — click for the release notes"
}

// releaseNotesURL points at this exact version's notes when the build came from a
// clean tag, and at the release list otherwise, since there is nothing to link a
// dev build to.
func releaseNotesURL() string {
	if strings.HasPrefix(version, "v") && !strings.ContainsAny(version, "-+") {
		return repoURL + "/releases/tag/" + version
	}
	return repoURL + "/releases"
}

// offerLaunchAtLogin asks — once — whether the app should open at login, and
// registers it as a login item if so. It stays quiet when there is nothing to
// offer: when the user has already answered either way, when the binary is not
// running from its .app bundle, or on a macOS without SMAppService.
func offerLaunchAtLogin() {
	if login.BundlePath() == "" || !login.Supported() {
		return
	}

	// Already answered: don't ask again, but do make the system match what we
	// recorded — see login.Reconcile for why that can't be read back instead.
	if login.Answered() {
		if login.Answer() {
			if login.Reconcile() {
				mLogin.Check()
			} else {
				mLogin.Uncheck()
			}
		}
		return
	}

	yes, err := notify.Confirm(
		"Launch Claude Usage at login?",
		"Claude Usage can start automatically when you log in, so your session and weekly meters are always in the menu bar.",
		"Launch at Login", "Not Now")
	if err != nil {
		return // the dialog itself failed; try again next launch rather than guess
	}
	if !yes {
		_ = login.RecordAnswer(false)
		return
	}

	if err := login.Enable(); err != nil {
		// Don't record the answer, so the offer is made again next launch.
		_ = notify.Banner("Claude Usage", "Could not open at login: "+err.Error())
		return
	}
	_ = login.RecordAnswer(true)
	mLogin.Check()
	warnIfNeedsApproval()
}

// toggleLaunchAtLogin flips the Launch at Login checkbox. The checkbox is only
// ticked once the change has actually gone through, so a failure leaves the menu
// showing what is really the case.
func toggleLaunchAtLogin() {
	if mLogin.Checked() {
		if err := login.Disable(); err != nil {
			_ = notify.Banner("Claude Usage", "Could not stop opening at login: "+err.Error())
			return
		}
		_ = login.RecordAnswer(false)
		mLogin.Uncheck()
		return
	}

	if err := login.Enable(); err != nil {
		_ = notify.Banner("Claude Usage", "Could not open at login: "+err.Error())
		return
	}
	_ = login.RecordAnswer(true)
	mLogin.Check()
	warnIfNeedsApproval()
}

// warnIfNeedsApproval covers the case where the user has previously switched the
// login item off in System Settings: registering succeeds, but macOS keeps it
// held back until they switch it on there.
func warnIfNeedsApproval() {
	if login.NeedsApproval() {
		_ = notify.Banner("Claude Usage",
			"Switch Claude Usage on under System Settings → General → Login Items to finish enabling it.")
	}
}

func pollLoop() {
	check() // immediate first check
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			check()
		case <-refreshNow:
			// "Refresh now" means now: drop the pacing so the usage endpoint is
			// actually called. If it refuses, the menu says so.
			nextUsage = time.Time{}
			usageBackoff = 0
			check()
		}
	}
}

func check() {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	// The status page needs no credentials and isn't rate limited, so check it
	// first and on every poll — the dot must keep working while the usage
	// endpoint is unavailable for any reason.
	checkStatus(ctx)

	// Respect the pacing the endpoint forces on us rather than calling it every
	// poll and collecting 429s.
	if time.Now().Before(nextUsage) {
		return
	}

	u, err := usage.Fetch(ctx)
	now := time.Now()
	stamp := now.Format("15:04:05")

	switch {
	case errors.Is(err, usage.ErrRateLimited):
		// Expected, not a fault: keep the numbers on screen and space out.
		var rl usage.RateLimited
		errors.As(err, &rl)
		wait := backOffUsage(rl.RetryAfter)
		nextUsage = now.Add(wait)
		mLastCheck.SetTitle("Rate limited; retrying " + nextUsage.Format("15:04:05"))
		mLastCheck.SetTooltip(fmt.Sprintf(
			"The usage endpoint allows about one request every two minutes. Backing off %s.",
			wait.Round(time.Second)))
		return

	case errors.Is(err, usage.ErrTokenExpired):
		// Keep the last known numbers; just flag that the token needs refreshing.
		systray.SetTitle("⚠")
		mSession.SetTitle("⚠ OAuth token expired")
		mWeekly.SetTitle("Run a Claude Code command to refresh")
		mSessionReset.Hide()
		mWeeklyReset.Hide()
		systray.SetTooltip("Claude usage — token expired; refresh via Claude Code")
		mLastCheck.SetTitle("Token expired " + stamp)
		mLastCheck.SetTooltip("")
		nextUsage = now.Add(usageInterval)
		return

	case err != nil:
		// Say what went wrong: a bare "failed" is indistinguishable from the
		// rate limiting above, which is what made this hard to diagnose.
		systray.SetTooltip("Claude usage — update failed")
		mLastCheck.SetTitle("Last check failed " + stamp + " — " + truncate(reason(err), 48))
		mLastCheck.SetTooltip(err.Error())
		nextUsage = now.Add(usageInterval)
		return
	}

	usageBackoff = 0
	nextUsage = now.Add(usageInterval)

	last = u
	haveData = true
	updateUI(u)
	notifyThresholds(u)
	mLastCheck.SetTitle("Last checked " + stamp)
	mLastCheck.SetTooltip("")
}

// backOffUsage doubles the wait each time the endpoint refuses, from one usage
// interval up to maxUsageBackoff, and takes the server's Retry-After instead when
// that asks for longer.
func backOffUsage(retryAfter time.Duration) time.Duration {
	if usageBackoff <= 0 {
		usageBackoff = usageInterval
	} else if usageBackoff < maxUsageBackoff {
		usageBackoff *= 2
	}
	if usageBackoff > maxUsageBackoff {
		usageBackoff = maxUsageBackoff
	}
	if retryAfter > usageBackoff {
		return retryAfter
	}
	return usageBackoff
}

// reason trims an error down to something that fits in a menu row: the first
// line, without the multi-line JSON body some failures carry.
func reason(err error) string {
	s := err.Error()
	if i := strings.IndexAny(s, "\n\r"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), ":"))
}

func updateUI(u usage.Usage) {
	systray.SetTitle(fmt.Sprintf("%d%% · %d%%", u.Session.Percent, u.Weekly.Percent))
	systray.SetTooltip(fmt.Sprintf("Claude usage — session %d%% · weekly %d%%", u.Session.Percent, u.Weekly.Percent))

	mSession.SetTitle(fmt.Sprintf("Current session: %d%%", u.Session.Percent))
	mWeekly.SetTitle(fmt.Sprintf("Weekly (all models): %d%%", u.Weekly.Percent))
	setReset(mSessionReset, u.Session)
	setReset(mWeeklyReset, u.Weekly)
}

func setReset(mi *systray.MenuItem, m usage.Meter) {
	if !m.HasReset {
		mi.Hide()
		return
	}
	mi.SetTitle("    resets " + humanizeReset(m.ResetsAt))
	mi.Show()
}

// humanizeReset formats a reset time as a short relative string for windows
// resetting within a day ("in 1h 59m"), or an absolute weekday/time otherwise
// ("Wed 22:59").
func humanizeReset(t time.Time) string {
	d := time.Until(t)
	switch {
	case d <= 0:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("in %dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("in %dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return "on " + t.Local().Format("Mon 15:04")
	}
}

// notifyThresholds fires a banner the first time a meter reaches alertPercent,
// re-arming once it drops back below (e.g. after a reset). The first poll only
// establishes a baseline so launching while already high doesn't spam.
func notifyThresholds(u usage.Usage) {
	if alertPercent <= 0 {
		return
	}
	sHigh := u.Session.Percent >= alertPercent
	wHigh := u.Weekly.Percent >= alertPercent

	if !firstRun {
		if sHigh && !alertedSession {
			_ = notify.Banner("Claude session usage high", fmt.Sprintf("Current session at %d%%", u.Session.Percent))
		}
		if wHigh && !alertedWeekly {
			_ = notify.Banner("Claude weekly usage high", fmt.Sprintf("Weekly (all models) at %d%%", u.Weekly.Percent))
		}
	}
	alertedSession = sHigh
	alertedWeekly = wHigh
	firstRun = false
}

// checkStatus polls status.claude.com and refreshes the dot and the incident
// rows. A failed poll keeps whatever was last known — a flaky network must not
// silently claim everything is fine.
func checkStatus(ctx context.Context) {
	s, err := status.Fetch(ctx, statusURL)
	if err != nil {
		if !haveStatus {
			mStatus.SetTitle("Service status unavailable")
		}
		return
	}
	haveStatus = true
	updateStatusUI(s)
	// Outside updateStatusUI on purpose: that holds mu, and this send can block
	// while a menu is open, which would stall the click handlers that need mu.
	reportStatusToIcon(s)
}

func updateStatusUI(s status.Summary) {
	mu.Lock()
	defer mu.Unlock()

	for i := range incidentURLs {
		incidentURLs[i] = ""
	}

	if s.AllOperational() {
		mStatus.SetTitle("✓  All services operational")
		mStatus.SetTooltip("status.claude.com reports all services operational")
		for _, mi := range mStatusItems {
			mi.Hide()
		}
		mAffected.Hide()
		return
	}

	desc := s.Description
	if desc == "" {
		desc = status.Human(s.Indicator) + " service disruption"
	}
	mStatus.SetTitle("●  " + desc)
	mStatus.SetTooltip("status.claude.com — click to open the status page")

	if extra := len(s.Incidents) - maxStatusItems; extra > 0 {
		mStatus.SetTitle(fmt.Sprintf("●  %s  (showing %d of %d)", desc, maxStatusItems, len(s.Incidents)))
	}

	for i, mi := range mStatusItems {
		if i >= len(s.Incidents) {
			mi.Hide()
			continue
		}
		inc := s.Incidents[i]
		label := inc.Name
		if inc.Status != "" {
			label = status.Human(inc.Status) + " — " + inc.Name
		}
		mi.SetTitle("    " + truncate(label, statusRowMaxLen))
		mi.SetTooltip(incidentTooltip(inc))
		incidentURLs[i] = inc.URL
		mi.Show()
	}

	if len(s.Affected) == 0 {
		mAffected.Hide()
		return
	}
	names := make([]string, 0, len(s.Affected))
	for _, c := range s.Affected {
		names = append(names, fmt.Sprintf("%s (%s)", c.Name, status.Human(c.Status)))
	}
	list := strings.Join(names, ", ")
	mAffected.SetTitle("    Affected: " + truncate(list, statusRowMaxLen))
	mAffected.SetTooltip("Affected: " + list)
	mAffected.Show()
}

func incidentTooltip(inc status.Incident) string {
	tip := inc.Name
	if inc.LatestUpdate != "" {
		tip += " — " + inc.LatestUpdate
	}
	return truncate(tip, 300)
}

// reportStatusToIcon tells iconLoop what the status page says: whether anything
// is wrong, and whether any of it is new since the last poll. An outage with no
// listed incidents still counts as ongoing — the indicator alone is enough.
func reportStatusToIcon(s status.Summary) {
	cur := make(map[string]bool, len(s.Incidents))
	isNew := false
	for _, inc := range s.Incidents {
		cur[inc.ID] = true
		if !lastIncidents[inc.ID] {
			isNew = true
		}
	}
	lastIncidents = cur
	iconUpdates <- iconUpdate{ongoing: !s.AllOperational(), isNew: isNew}
}

// iconUpdate is what the latest status poll saw.
type iconUpdate struct{ ongoing, isNew bool }

type iconState int

const (
	stateOK    iconState = iota // all operational: no icon at all, text only
	stateAlert                  // unacknowledged outage: blinking red dot
	stateAck                    // seen by the user: steady red dot
)

// iconLoop owns the menu-bar icon. It is the only writer, so the blink can never
// race a state change and leave the wrong icon on screen. The dot blinks until
// the user opens the menu, then stays solid for as long as the outage lasts.
func iconLoop() {
	st := stateOK
	red := false

	setRed := func(on bool) {
		red = on
		if on {
			systray.SetIcon(icon.RedDotPNG())
		} else {
			// Blink off, not outage over: a transparent image of the same size
			// holds the icon slot so the percentages don't shift.
			systray.SetIcon(icon.BlankPNG())
		}
	}
	apply := func(next iconState) {
		st = next
		switch next {
		case stateOK:
			// All clear: drop the icon entirely, so it's text only again.
			red = false
			systray.SetIcon(noIcon)
		case stateAlert:
			setRed(true) // start the blink lit, so it is seen immediately
		case stateAck:
			red = true
			systray.SetIcon(icon.RedDotPNG())
		}
	}

	t := time.NewTicker(blinkInterval)
	defer t.Stop()

	for {
		select {
		case u := <-iconUpdates:
			switch {
			case !u.ongoing:
				if st != stateOK {
					apply(stateOK)
				}
			case u.isNew || st == stateOK:
				// A fresh incident re-arms the blink even if an earlier one has
				// already been acknowledged.
				apply(stateAlert)
			}
		case <-iconAck:
			if st == stateAlert {
				apply(stateAck)
			}
		case <-t.C:
			if st == stateAlert {
				setRed(!red)
			}
		}
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n < 1 {
		return ""
	}
	return string(r[:n-1]) + "…"
}

func incidentURL(i int) string {
	mu.Lock()
	defer mu.Unlock()
	if i < 0 || i >= len(incidentURLs) {
		return ""
	}
	return incidentURLs[i]
}

func triggerRefresh() {
	select {
	case refreshNow <- struct{}{}:
	default: // a refresh is already queued
	}
}

func openURL(u string) {
	if u == "" {
		return
	}
	_ = exec.Command("open", u).Start()
}

// usageIntervalFromEnv reads USAGE_SECONDS, refusing anything below
// minUsageInterval: going faster than the endpoint's own limit only produces
// 429s, so a smaller value would make the meter worse, not fresher.
func usageIntervalFromEnv() time.Duration {
	if v := os.Getenv("USAGE_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if d := time.Duration(n) * time.Second; d >= minUsageInterval {
				return d
			}
		}
	}
	return defaultUsageInterval
}

func pollIntervalFromEnv() time.Duration {
	if v := os.Getenv("POLL_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 10 {
			return time.Duration(n) * time.Second
		}
	}
	return defaultPollInterval
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func alertPercentFromEnv() int {
	if v := os.Getenv("ALERT_PERCENT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 100 {
			return n
		}
	}
	return 80
}
