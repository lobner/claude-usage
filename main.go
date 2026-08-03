// Command claudeusage is a macOS menu-bar app that shows your Claude subscription
// usage as two percentages in the menu-bar title — "<session>% · <weekly>%",
// where session is the current 5-hour window and weekly is the 7-day "all models"
// limit — the same numbers as claude.ai/settings/usage. It reads the usage
// endpoint directly with Claude Code's stored OAuth token once a minute and posts
// a Notification Centre banner when a meter crosses a threshold.
//
// It also polls status.claude.com: whenever the status page reports anything
// other than "All Systems Operational", a red dot appears in front of the
// percentages and the dropdown lists the ongoing incidents.
//
// Environment overrides:
//
//	POLL_SECONDS   polling interval in seconds (default 60, minimum 10)
//	ALERT_PERCENT  notify when a meter first reaches this % (default 80, 0=off)
//	STATUS_URL     Statuspage summary document to poll (default status.claude.com)
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
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
	usagePageURL        = "https://claude.ai/settings/usage"
	statusPageURL       = "https://status.claude.com"
	maxStatusItems      = 5  // fixed pool of incident rows (systray can't remove items)
	statusRowMaxLen     = 64 // characters per incident row
)

// noIcon is deliberately undecodable image data: systray has no "remove the
// icon" call, but AppKit turns data it cannot decode into a nil NSImage, which
// drops the image from the status item (leaving the title text alone).
var noIcon = []byte{0}

var (
	pollInterval = pollIntervalFromEnv()
	alertPercent = alertPercentFromEnv()
	statusURL    = envOr("STATUS_URL", status.DefaultURL)

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

	// Poll-goroutine-only state.
	last           usage.Usage
	haveData       bool
	firstRun       = true
	alertedSession bool
	alertedWeekly  bool
	haveStatus     bool
	dotShown       bool

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
		<-mQuit.ClickedCh
		systray.Quit()
	}()

	go pollLoop()
	go offerLaunchAtLogin()
}

func onExit() {}

// offerLaunchAtLogin asks — once — whether the app should open at login, and
// registers it as a login item if so. It stays quiet when there is nothing to
// offer: when the user has already answered either way, when the binary is not
// running from its .app bundle, or on a macOS without SMAppService.
func offerLaunchAtLogin() {
	if login.BundlePath() == "" || !login.Supported() || login.Answered() {
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
			check()
		}
	}
}

func check() {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	// The status page needs no credentials, so check it first and independently
	// of the usage endpoint — the dot must still work while the token is expired.
	checkStatus(ctx)

	u, err := usage.Fetch(ctx)
	now := time.Now().Format("15:04:05")

	switch {
	case err == usage.ErrTokenExpired:
		// Keep the last known numbers; just flag that the token needs refreshing.
		systray.SetTitle("⚠")
		mSession.SetTitle("⚠ OAuth token expired")
		mWeekly.SetTitle("Run a Claude Code command to refresh")
		mSessionReset.Hide()
		mWeeklyReset.Hide()
		systray.SetTooltip("Claude usage — token expired; refresh via Claude Code")
		mLastCheck.SetTitle("Token expired " + now)
		return
	case err != nil:
		systray.SetTooltip("Claude usage — update failed")
		mLastCheck.SetTitle("Last check failed " + now)
		return
	}

	last = u
	haveData = true
	updateUI(u)
	notifyThresholds(u)
	mLastCheck.SetTitle("Last checked " + now)
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
}

func updateStatusUI(s status.Summary) {
	mu.Lock()
	defer mu.Unlock()

	for i := range incidentURLs {
		incidentURLs[i] = ""
	}

	if s.AllOperational() {
		setDot(false)
		mStatus.SetTitle("✓  All services operational")
		mStatus.SetTooltip("status.claude.com reports all services operational")
		for _, mi := range mStatusItems {
			mi.Hide()
		}
		mAffected.Hide()
		return
	}

	setDot(true)
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

// setDot shows or hides the red dot in front of the menu-bar percentages. The
// systray call reaches AppKit on the main thread, so it is only made when the
// state actually changes.
func setDot(on bool) {
	if on == dotShown {
		return
	}
	if on {
		systray.SetIcon(icon.RedDotPNG())
	} else {
		systray.SetIcon(noIcon)
	}
	dotShown = on
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
