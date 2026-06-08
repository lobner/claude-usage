// Command claudeusage is a macOS menu-bar app that shows your Claude subscription
// usage as two vertical 0–100% meter bars (left = current 5-hour session, right =
// the 7-day "all models" weekly limit) — the same numbers as
// claude.ai/settings/usage. It reads the usage endpoint directly with Claude
// Code's stored OAuth token once a minute and posts a Notification Centre banner
// when a meter crosses a threshold.
//
// Environment overrides:
//
//	POLL_SECONDS   polling interval in seconds (default 60, minimum 10)
//	ALERT_PERCENT  notify when a meter first reaches this % (default 80, 0=off)
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"

	"fyne.io/systray"

	"claudeusage/internal/icon"
	"claudeusage/internal/notify"
	"claudeusage/internal/usage"
)

const (
	defaultPollInterval = 60 * time.Second
	usagePageURL        = "https://claude.ai/settings/usage"
)

var (
	pollInterval = pollIntervalFromEnv()
	alertPercent = alertPercentFromEnv()

	mSession      *systray.MenuItem
	mSessionReset *systray.MenuItem
	mWeekly       *systray.MenuItem
	mWeeklyReset  *systray.MenuItem
	mLastCheck    *systray.MenuItem

	refreshNow = make(chan struct{}, 1)

	// Poll-goroutine-only state.
	last           usage.Usage
	haveData       bool
	firstRun       = true
	alertedSession bool
	alertedWeekly  bool
)

func main() {
	systray.Run(onReady, onExit)
}

func onReady() {
	systray.SetTemplateIcon(icon.BarsPNG(0, 0), icon.BarsPNG(0, 0))
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
	mOpenPage := systray.AddMenuItem("Open usage page", "Open claude.ai/settings/usage")
	mRefresh := systray.AddMenuItem("Refresh now", "Fetch usage immediately")
	mLastCheck = systray.AddMenuItem("", "")
	mLastCheck.Disable()
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit", "Quit Claude Usage")

	go func() {
		for range mOpenPage.ClickedCh {
			openURL(usagePageURL)
		}
	}()
	go func() {
		for range mRefresh.ClickedCh {
			triggerRefresh()
		}
	}()
	go func() {
		<-mQuit.ClickedCh
		systray.Quit()
	}()

	go pollLoop()
}

func onExit() {}

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

	u, err := usage.Fetch(ctx)
	now := time.Now().Format("15:04:05")

	switch {
	case err == usage.ErrTokenExpired:
		// Keep the last known bars; just flag that the token needs refreshing.
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
	png := icon.BarsPNG(float64(u.Session.Percent), float64(u.Weekly.Percent))
	systray.SetTemplateIcon(png, png)
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

func triggerRefresh() {
	select {
	case refreshNow <- struct{}{}:
	default: // a refresh is already queued
	}
}

func openURL(u string) {
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

func alertPercentFromEnv() int {
	if v := os.Getenv("ALERT_PERCENT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 100 {
			return n
		}
	}
	return 80
}
