// Package notify posts macOS Notification Centre banners via osascript, which
// needs no app bundle. (When the binary is wrapped in a .app the banner is
// attributed to that app instead of to "Script Editor".)
package notify

import (
	"os/exec"
	"strings"
)

// Banner shows a notification with the given title and body. Errors are returned
// but are safe to ignore — a missing notification must never crash the tracker.
func Banner(title, body string) error {
	script := "display notification " + quote(body) + " with title " + quote(title)
	return exec.Command("osascript", "-e", script).Run()
}

// quote turns s into a safe AppleScript double-quoted string literal.
func quote(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"\n", " ",
		"\r", " ",
	)
	return `"` + r.Replace(s) + `"`
}
