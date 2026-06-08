// Package usage fetches the two headline meters shown on claude.ai/settings/usage
// — the 5-hour "current session" window and the 7-day "all models" weekly window
// — directly from Anthropic's OAuth usage endpoint, using Claude Code's stored
// OAuth token.
//
// It is READ-ONLY: it reads the token (from the macOS Keychain, falling back to
// ~/.claude/.credentials.json) but never refreshes or writes it. The token
// expires roughly hourly and is refreshed only by running the `claude` CLI; once
// it has expired, Fetch returns ErrTokenExpired until the next refresh.
//
// There is no officially documented API for this data — the endpoint is the same
// internal one the claude.ai usage page calls. See:
//
//	https://github.com/anthropics/claude-code/issues/13585
package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	usageURL        = "https://api.anthropic.com/api/oauth/usage"
	betaHeader      = "oauth-2025-04-20" // required, else the endpoint 401s
	keychainService = "Claude Code-credentials"
)

// ErrTokenExpired means the stored OAuth token is expired or was rejected.
// Callers should keep the last known meters and prompt the user to refresh
// (run any Claude Code command), rather than treat it as a hard error.
var ErrTokenExpired = errors.New("oauth token expired")

// Meter is a single usage window.
type Meter struct {
	Percent  int       // 0–100, rounded to match the usage page
	ResetsAt time.Time // when the window resets; zero if unknown
	HasReset bool
}

// Usage is the pair of meters the menu bar renders.
type Usage struct {
	Session Meter // five_hour  (left bar)
	Weekly  Meter // seven_day  (right bar)
}

var httpClient = &http.Client{Timeout: 20 * time.Second}

// Fetch reads the stored OAuth token and queries the usage endpoint.
func Fetch(ctx context.Context) (Usage, error) {
	cred, err := loadCredentials(ctx)
	if err != nil {
		return Usage{}, err
	}
	// Don't hammer the endpoint with a known-dead token: if it has already
	// expired locally, report it without a doomed request. A live 401 is still
	// handled in fetch(), in case expiresAt is missing or wrong.
	if !cred.expiresAt.IsZero() && time.Now().After(cred.expiresAt) {
		return Usage{}, ErrTokenExpired
	}
	return fetch(ctx, cred.accessToken)
}

func fetch(ctx context.Context, token string) (Usage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageURL, nil)
	if err != nil {
		return Usage{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", betaHeader)

	resp, err := httpClient.Do(req)
	if err != nil {
		return Usage{}, fmt.Errorf("usage request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return Usage{}, ErrTokenExpired
	case resp.StatusCode != http.StatusOK:
		return Usage{}, fmt.Errorf("usage endpoint HTTP %d: %s", resp.StatusCode, snippet(body))
	}
	return parse(body)
}

type window struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

type apiResponse struct {
	Type  string `json:"type"` // "error" on the error envelope, absent on success
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
	FiveHour *window `json:"five_hour"`
	SevenDay *window `json:"seven_day"`
}

func parse(body []byte) (Usage, error) {
	var r apiResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return Usage{}, fmt.Errorf("parsing usage JSON: %w", err)
	}
	if r.Type == "error" {
		if r.Error != nil && r.Error.Type == "authentication_error" {
			return Usage{}, ErrTokenExpired
		}
		if r.Error != nil && r.Error.Message != "" {
			return Usage{}, errors.New(r.Error.Message)
		}
		return Usage{}, errors.New("usage endpoint returned an error")
	}
	return Usage{Session: toMeter(r.FiveHour), Weekly: toMeter(r.SevenDay)}, nil
}

func toMeter(w *window) Meter {
	var m Meter
	if w == nil || w.Utilization == nil {
		return m
	}
	p := int(math.Round(*w.Utilization))
	switch {
	case p < 0:
		p = 0
	case p > 100:
		p = 100
	}
	m.Percent = p
	if w.ResetsAt != nil && *w.ResetsAt != "" {
		if t, err := time.Parse(time.RFC3339, *w.ResetsAt); err == nil {
			m.ResetsAt = t
			m.HasReset = true
		}
	}
	return m
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// ---- credentials (read-only) ----

type credentials struct {
	accessToken string
	expiresAt   time.Time // zero if unknown
}

// credBlob is the on-disk / Keychain shape; we read only what we need.
type credBlob struct {
	ClaudeAiOauth struct {
		AccessToken string `json:"accessToken"`
		ExpiresAt   int64  `json:"expiresAt"` // epoch milliseconds
	} `json:"claudeAiOauth"`
}

// loadCredentials reads Claude Code's OAuth credentials from the macOS Keychain,
// falling back to ~/.claude/.credentials.json.
func loadCredentials(ctx context.Context) (credentials, error) {
	if blob, err := keychainRead(ctx); err == nil {
		if c, ok := parseBlob(blob); ok {
			return c, nil
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		path := filepath.Join(home, ".claude", ".credentials.json")
		if data, err := os.ReadFile(path); err == nil {
			if c, ok := parseBlob(data); ok {
				return c, nil
			}
		}
	}
	return credentials{}, fmt.Errorf(
		"no Claude Code OAuth credentials found (Keychain item %q or ~/.claude/.credentials.json); log in with `claude` first",
		keychainService,
	)
}

// keychainRead shells out to the system `security` tool. Its absolute path is
// used because a GUI/launchd-started app inherits a minimal PATH (which still
// includes /usr/bin, where `security` always lives).
func keychainRead(ctx context.Context) ([]byte, error) {
	return exec.CommandContext(ctx, "/usr/bin/security",
		"find-generic-password", "-s", keychainService, "-w").Output()
}

func parseBlob(data []byte) (credentials, bool) {
	var b credBlob
	if err := json.Unmarshal(data, &b); err != nil {
		return credentials{}, false
	}
	if b.ClaudeAiOauth.AccessToken == "" {
		return credentials{}, false
	}
	c := credentials{accessToken: b.ClaudeAiOauth.AccessToken}
	if b.ClaudeAiOauth.ExpiresAt > 0 {
		c.expiresAt = time.UnixMilli(b.ClaudeAiOauth.ExpiresAt)
	}
	return c, true
}
