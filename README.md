# Claude Usage — macOS menu-bar meter

A tiny menu-bar app that shows your Claude subscription usage as **two
percentages** in the menu-bar title — `<session>% · <weekly>%`, the same numbers
as [claude.ai/settings/usage](https://claude.ai/settings/usage):

- **First number** → current **session** (the rolling 5-hour window).
- **Second number** → **weekly**, all models (the rolling 7-day window).

The dropdown shows each percentage, its reset time, *Open usage page*,
*Refresh now*, the last-checked time, and *Quit*. When a meter first crosses a
threshold (default 80%) you get a Notification Centre banner.

## How it works

Every 60 s it queries `https://api.anthropic.com/api/oauth/usage` directly —
the same internal endpoint the claude.ai usage page calls — using Claude Code's
stored OAuth token (read from the macOS Keychain, falling back to
`~/.claude/.credentials.json`). It reads the two windows by their **stable**
upstream keys — `five_hour` and `seven_day` — so renamed labels or new internal
codename windows never break it. (There is no officially documented API for this;
see [anthropics/claude-code#13585](https://github.com/anthropics/claude-code/issues/13585).)

## ⚠️ OAuth token & refresh — read this

The app is **read-only**: it uses whatever OAuth token Claude Code has stored
and **never refreshes it**. That token expires roughly hourly, and only running
the `claude` CLI refreshes it. So:

- While the token is valid, the numbers update every minute as expected.
- Once it expires, every poll returns "token expired", the title shows **"⚠"**,
  the numbers stop updating, and the menu shows **"⚠ OAuth token expired — run a
  Claude Code command to refresh"** until you next use Claude Code.

For an always-on meter this is the main limitation. The fix would be an
auto-refresh mode (refresh the token via the OAuth refresh endpoint and write it
back to the Keychain).

## Build & run

Requires Go 1.22+ (this repo pins `golang 1.25.11` via `.tool-versions`).

```sh
# Run in the foreground (Ctrl-C to stop):
go run .

# Or build a no-dock .app you can double-click / add to Login Items:
./build/make-app.sh
open "Claude Usage.app"
```

`make-app.sh` produces `Claude Usage.app` with `LSUIElement=true` (menu-bar-only,
no Dock icon).

## Configuration

| Env var         | Default | Meaning                                                        |
| --------------- | ------- | -------------------------------------------------------------- |
| `POLL_SECONDS`  | `60`    | Polling interval in seconds (min 10).                          |
| `ALERT_PERCENT` | `80`    | Banner when a meter first reaches this %. `0` disables alerts. |

## Launch at login

Either:

- **Login Items** — System Settings → General → Login Items → add
  `Claude Usage.app`; or
- **LaunchAgent** — copy `build/dk.biq.claudeusage.plist` to
  `~/Library/LaunchAgents/`, fix the path inside if the app isn't in
  `/Applications`, then
  `launchctl load ~/Library/LaunchAgents/dk.biq.claudeusage.plist`.

## Project layout

```
main.go                 systray wiring, poll loop, menu, threshold banners
internal/usage/         read OAuth token + query endpoint, parse stable keys → two meters
internal/notify/        Notification Centre banner via osascript
build/                  Info.plist, make-app.sh, LaunchAgent plist
```

## Testing

```sh
go test ./...   # unit tests (offline)
```

## Notes & possible extensions

- **Auto-refresh** (above) is the most useful next step for unattended running.
- **Two-line display** — MenuMeters-style stacked numbers aren't possible through
  `fyne.io/systray` (it force-sizes icon images to 16×16), so the title is a
  single line. A stacked variant would need a custom `NSStatusItem` view, i.e. a
  fork of the systray library.
- **More windows** — the endpoint also returns Sonnet/Opus and rotating internal
  codename windows; the title could grow more numbers, but two keeps the
  menu-bar footprint small.
