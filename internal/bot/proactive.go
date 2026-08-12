package bot

import (
	"strings"
	"time"

	"github.com/openilink/openilink-hub/internal/store"
)

// gateResult is the outcome of the proactive anti-disturbance gate.
type gateResult struct {
	Allow  bool
	Reason string // non-empty when cancelled
}

// gateProactive is the pure-function anti-disturbance gate (design §8.4).
// It only ever blocks origin=proactive messages; origin=promise (user
// promises) always passes straight through — the user asked for it.
//
// Inputs:
//   - msg: the due scheduled message
//   - lastUserMsgAt: unix seconds of the last inbound message from that person
//   - now: current time
//   - cfg: resolved AI config
//   - sentLog: previously sent proactive messages (for daily cap & interval)
//
// 热聊抑制 is NOT decided here: the gap to the last user message is passed to
// the AI for a second opinion at send time (§8.2).
func gateProactive(msg *store.ScheduledMessage, lastUserMsgAt int64, now time.Time, cfg store.AIConfig, sentLog []store.ScheduledMessage) gateResult {
	if msg.Origin != "proactive" {
		return gateResult{Allow: true}
	}
	if !cfg.ProactiveEnabled {
		return gateResult{Allow: false, Reason: "proactive 已关闭"}
	}

	// 静默时段 (quiet hours): block when the current time falls inside it.
	if withinQuietHours(now, cfg.ProactiveQuietHours) {
		return gateResult{Allow: false, Reason: "静默时段"}
	}

	// Count only proactive sends (promise never consumes quota — design §8.4).
	var todaySent int
	var lastSentAt int64
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
	for _, sm := range sentLog {
		if sm.Origin != "proactive" || sm.Status != "sent" {
			continue
		}
		if sm.SentAt != nil {
			if *sm.SentAt >= dayStart {
				todaySent++
			}
			if *sm.SentAt > lastSentAt {
				lastSentAt = *sm.SentAt
			}
		}
	}

	// 每日条数上限.
	limit := cfg.ProactiveDailyLimit
	if limit <= 0 {
		limit = 3
	}
	if todaySent >= limit {
		return gateResult{Allow: false, Reason: "每日上限"}
	}

	// 最小间隔.
	minInterval := int64(cfg.ProactiveMinInterval)
	if minInterval <= 0 {
		minInterval = 7200
	}
	if lastSentAt > 0 && now.Unix()-lastSentAt < minInterval {
		return gateResult{Allow: false, Reason: "最小间隔"}
	}

	return gateResult{Allow: true}
}

// withinQuietHours reports whether t falls inside a "HH:MM-HH:MM" window that
// may wrap past midnight (e.g. "23:00-08:00").
func withinQuietHours(t time.Time, window string) bool {
	window = strings.TrimSpace(window)
	if window == "" {
		return false
	}
	parts := strings.SplitN(window, "-", 2)
	if len(parts) != 2 {
		return false
	}
	start, ok1 := parseHM(parts[0], t)
	end, ok2 := parseHM(parts[1], t)
	if !ok1 || !ok2 {
		return false
	}
	ts := t.Unix()
	// Normalize: if the window wraps midnight, split into two windows.
	if end.Before(start) {
		// [start, 24:00) ∪ [00:00, end)
		midnightNext := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()).AddDate(0, 0, 1)
		if ts >= start.Unix() && ts < midnightNext.Unix() {
			return true
		}
		dayStart := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
		return ts >= dayStart.Unix() && ts < end.Unix()
	}
	return ts >= start.Unix() && ts < end.Unix()
}

func parseHM(s string, base time.Time) (time.Time, bool) {
	s = strings.TrimSpace(s)
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return time.Time{}, false
	}
	var h, m int
	if _, err := fmtSscan(parts[0], &h); err != nil {
		return time.Time{}, false
	}
	if _, err := fmtSscan(parts[1], &m); err != nil {
		return time.Time{}, false
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return time.Time{}, false
	}
	// Anchor the window to the SAME day as the time being checked (not "now"),
	// so cross-day window checks are date-independent.
	return time.Date(base.Year(), base.Month(), base.Day(), h, m, 0, 0, base.Location()), true
}

func fmtSscan(s string, out *int) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errParseHM
		}
		n = n*10 + int(c-'0')
	}
	*out = n
	return n, nil
}

var errParseHM = &parseHMError{}

type parseHMError struct{}

func (*parseHMError) Error() string { return "invalid time" }
