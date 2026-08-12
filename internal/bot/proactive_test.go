package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openilink/openilink-hub/internal/persona"
	"github.com/openilink/openilink-hub/internal/provider"
	"github.com/openilink/openilink-hub/internal/sink"
	"github.com/openilink/openilink-hub/internal/store"
	"github.com/openilink/openilink-hub/internal/store/sqlite"
)

func mustSentMsg(id string, sentAt int64) store.ScheduledMessage {
	return store.ScheduledMessage{ID: id, Origin: "proactive", Status: "sent", SentAt: &sentAt}
}

func TestGateProactive_PromiseAlwaysPasses(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.Local)
	cfg := store.AIConfig{ProactiveEnabled: false} // even with the master switch off
	msg := &store.ScheduledMessage{Origin: "promise"}
	g := gateProactive(msg, 0, now, cfg, nil)
	if !g.Allow {
		t.Errorf("promise must pass straight through, got cancelled: %q", g.Reason)
	}
}

func TestGateProactive_Disabled(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.Local)
	cfg := store.AIConfig{ProactiveEnabled: false}
	msg := &store.ScheduledMessage{Origin: "proactive"}
	g := gateProactive(msg, 0, now, cfg, nil)
	if g.Allow {
		t.Error("proactive should be blocked when proactive_enabled=false")
	}
	if g.Reason != "proactive 已关闭" {
		t.Errorf("reason = %q", g.Reason)
	}
}

func TestGateProactive_QuietHours(t *testing.T) {
	cfg := store.AIConfig{
		ProactiveEnabled:     true,
		ProactiveQuietHours:  "23:00-08:00",
		ProactiveDailyLimit:  3,
		ProactiveMinInterval: 7200,
	}
	cases := []struct {
		name string
		now  time.Time
		want bool
	}{
		{"night", time.Date(2026, 8, 11, 23, 30, 0, 0, time.Local), false},
		{"deep night", time.Date(2026, 8, 11, 2, 0, 0, 0, time.Local), false},
		{"boundary start", time.Date(2026, 8, 11, 23, 0, 0, 0, time.Local), false},
		{"morning before end", time.Date(2026, 8, 11, 7, 59, 0, 0, time.Local), false},
		{"boundary end", time.Date(2026, 8, 11, 8, 0, 0, 0, time.Local), true},
		{"daytime", time.Date(2026, 8, 11, 12, 0, 0, 0, time.Local), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := &store.ScheduledMessage{Origin: "proactive"}
			g := gateProactive(msg, 0, tc.now, cfg, nil)
			if g.Allow != tc.want {
				t.Errorf("allow = %v, want %v (reason %q)", g.Allow, tc.want, g.Reason)
			}
			if !tc.want && g.Reason == "" {
				t.Error("cancelled without a reason")
			}
		})
	}
}

func TestGateProactive_DailyLimit(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.Local)
	cfg := store.AIConfig{ProactiveEnabled: true, ProactiveDailyLimit: 3, ProactiveMinInterval: 7200}
	msg := &store.ScheduledMessage{Origin: "proactive"}

	// 3 sent today → blocked.
	log3 := []store.ScheduledMessage{
		mustSentMsg("a", now.Unix()-1000),
		mustSentMsg("b", now.Unix()-2000),
		mustSentMsg("c", now.Unix()-3000),
	}
	g := gateProactive(msg, 0, now, cfg, log3)
	if g.Allow {
		t.Error("expected blocked at daily limit")
	}
	if g.Reason != "每日上限" {
		t.Errorf("reason = %q", g.Reason)
	}

	// 2 sent today + 1 promise sent → promise does NOT consume proactive quota.
	// Proactive sends are > 2h ago so the min-interval rule does not interfere.
	promiseLog := []store.ScheduledMessage{
		mustSentMsg("a", now.Unix()-3*3600),
		mustSentMsg("b", now.Unix()-4*3600),
		{ID: "p", Origin: "promise", Status: "sent", SentAt: int64Ptr(now.Unix() - 500)},
	}
	g = gateProactive(msg, 0, now, cfg, promiseLog)
	if !g.Allow {
		t.Errorf("promise sends must not consume proactive quota, got %q", g.Reason)
	}
}

func TestGateProactive_MinInterval(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.Local)
	cfg := store.AIConfig{ProactiveEnabled: true, ProactiveDailyLimit: 3, ProactiveMinInterval: 7200}
	msg := &store.ScheduledMessage{Origin: "proactive"}

	// Last proactive sent 1h ago → blocked (min interval 2h).
	recent := []store.ScheduledMessage{mustSentMsg("a", now.Unix()-3600)}
	g := gateProactive(msg, 0, now, cfg, recent)
	if g.Allow {
		t.Error("expected blocked below min interval")
	}
	if g.Reason != "最小间隔" {
		t.Errorf("reason = %q", g.Reason)
	}

	// Exactly at threshold → allowed.
	exact := []store.ScheduledMessage{mustSentMsg("a", now.Unix()-7200)}
	if g := gateProactive(msg, 0, now, cfg, exact); !g.Allow {
		t.Error("expected allowed exactly at min interval")
	}
}

func TestWithinQuietHours(t *testing.T) {
	loc := time.Local
	cases := []struct {
		window string
		t      time.Time
		want   bool
	}{
		{"23:00-08:00", time.Date(2026, 8, 11, 22, 59, 0, 0, loc), false},
		{"23:00-08:00", time.Date(2026, 8, 11, 23, 0, 0, 0, loc), true},
		{"23:00-08:00", time.Date(2026, 8, 11, 8, 0, 0, 0, loc), false},
		{"23:00-08:00", time.Date(2026, 8, 11, 7, 0, 0, 0, loc), true},
		{"09:00-17:00", time.Date(2026, 8, 11, 12, 0, 0, 0, loc), true},
		{"09:00-17:00", time.Date(2026, 8, 11, 18, 0, 0, 0, loc), false},
		{"bad-window", time.Date(2026, 8, 11, 12, 0, 0, 0, loc), false},
		{"", time.Date(2026, 8, 11, 12, 0, 0, 0, loc), false},
	}
	for _, tc := range cases {
		if got := withinQuietHours(tc.t, tc.window); got != tc.want {
			t.Errorf("withinQuietHours(%s, %q) = %v, want %v", tc.t.Format("15:04"), tc.window, got, tc.want)
		}
	}
}

func int64Ptr(v int64) *int64 { return &v }

// TestProcessDueScheduledMessage_ReschedulesProactiveWhenGated covers the
// Task 6 wiring: when the anti-disturbance gate blocks an origin=proactive
// message (cfg passed as store.AIConfig{} ⇒ ProactiveEnabled=false), the
// manager asks the AI sink for a new fire time. The original row must be
// cancelled with reason 重定 and a NEW pending proactive row must exist with
// RescheduleCount bumped to 1 and FireAt matching the mock LLM's answer.
func TestProcessDueScheduledMessage_ReschedulesProactiveWhenGated(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "proactive_resched.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	user, err := db.CreateUser("sched_resched_owner", "Sched Resched")
	if err != nil {
		t.Fatal(err)
	}
	botRow, err := db.CreateBot(user.ID, "ReschedBot", "test", "wx_bot", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	// 24h WeChat session window: a fresh inbound context token is required.
	if _, err := db.SaveMessage(&store.Message{
		BotID:        botRow.ID,
		Direction:    "inbound",
		FromUserID:   "wxid_a",
		ToUserID:     "wx_bot",
		ContextToken: "ctx-1",
		ItemList:     json.RawMessage(`[{"type":"text","text":"hi"}]`),
	}); err != nil {
		t.Fatal(err)
	}

	// Mock LLM returns a concrete future time (~1 day out) so the rescheduled
	// fire_at is computable in the test without hardcoding a date.
	future := time.Now().Add(24 * time.Hour)
	futureStr := future.Format("2006-01-02 15:04")
	wantAt := future.Truncate(time.Minute).Unix()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]any{"role": "assistant", "content": futureStr},
				"finish_reason": "stop",
			}},
		})
	}))
	defer srv.Close()

	for k, v := range map[string]string{
		"ai.api_key":           "test-key",
		"ai.base_url":          srv.URL,
		"ai.model":             "test-model",
		"ai.proactive_enabled": "false",
	} {
		if err := db.SetConfig(k, v); err != nil {
			t.Fatal(err)
		}
	}

	msg := &store.ScheduledMessage{
		BotID:     botRow.ID,
		Sender:    "wxid_a",
		FireAt:    time.Now().Add(-time.Minute).Unix(),
		Tolerance: 3600, // high so the tolerance check does not preempt the gate
		Prompt:    "想你了",
		Origin:    "proactive",
		Status:    "pending",
	}
	if err := db.AddScheduledMessage(msg); err != nil {
		t.Fatal(err)
	}

	m := &Manager{
		store:     db,
		aiSink:    &sink.AI{Store: db},
		instances: map[string]*Instance{botRow.ID: NewInstance(botRow.ID, &fakeProvider{})},
	}
	// cfg passed as store.AIConfig{} ⇒ gateProactive sees ProactiveEnabled=false → gated.
	m.processDueScheduledMessage(msg, store.AIConfig{}, time.Now())

	all, err := db.ListScheduledMessages(botRow.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	var orig *store.ScheduledMessage
	var newPending *store.ScheduledMessage
	for i := range all {
		if all[i].ID == msg.ID {
			orig = &all[i]
		}
		if all[i].ID != msg.ID && all[i].Sender == "wxid_a" && all[i].Origin == "proactive" && all[i].Status == "pending" {
			newPending = &all[i]
		}
	}
	if orig == nil {
		t.Fatalf("original message %s not found", msg.ID)
	}
	if orig.Status != "cancelled" {
		t.Fatalf("original status = %q, want cancelled", orig.Status)
	}
	if orig.FailReason != "重定" {
		t.Fatalf("original fail_reason = %q, want 重定", orig.FailReason)
	}
	if newPending == nil {
		t.Fatal("expected a new pending proactive message after reschedule")
	}
	if newPending.RescheduleCount != 1 {
		t.Fatalf("new pending reschedule_count = %d, want 1", newPending.RescheduleCount)
	}
	if newPending.FireAt != wantAt {
		t.Fatalf("new pending fire_at = %d, want %d (%s)", newPending.FireAt, wantAt, futureStr)
	}
}

func TestProcessDueScheduledMessage_CancelsWhenPastTolerance(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "proactive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	user, err := db.CreateUser("sched_owner", "Sched Owner")
	if err != nil {
		t.Fatal(err)
	}
	botRow, err := db.CreateBot(user.ID, "SchedBot", "test", "wx_bot", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveMessage(&store.Message{
		BotID:        botRow.ID,
		Direction:    "inbound",
		FromUserID:   "wxid_a",
		ToUserID:     "wx_bot",
		ContextToken: "ctx-1",
		ItemList:     json.RawMessage(`[{"type":"text","text":"hi"}]`),
	}); err != nil {
		t.Fatal(err)
	}

	msg := &store.ScheduledMessage{
		BotID:     botRow.ID,
		Sender:    "wxid_a",
		FireAt:    time.Now().Add(-30 * time.Minute).Unix(),
		Tolerance: 60,
		Prompt:    "已经过期了",
		Origin:    "promise",
		Status:    "pending",
	}
	if err := db.AddScheduledMessage(msg); err != nil {
		t.Fatal(err)
	}

	m := &Manager{
		store:     db,
		aiSink:    &sink.AI{},
		instances: map[string]*Instance{botRow.ID: NewInstance(botRow.ID, &fakeProvider{})},
	}

	m.processDueScheduledMessage(msg, store.AIConfig{}, time.Now())

	all, err := db.ListScheduledMessages(botRow.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range all {
		if got.ID == msg.ID {
			if got.Status != "cancelled" {
				t.Fatalf("status = %q, want cancelled", got.Status)
			}
			if got.FailReason != "超出容差" {
				t.Fatalf("fail_reason = %q, want 超出容差", got.FailReason)
			}
			return
		}
	}
	t.Fatalf("scheduled message %s not found", msg.ID)
}

type captureSlogHandler struct {
	fn func(rec slog.Record)
}

func (h *captureSlogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureSlogHandler) Handle(ctx context.Context, rec slog.Record) error {
	h.fn(rec)
	return nil
}
func (h *captureSlogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureSlogHandler) WithGroup(string) slog.Handler      { return h }

func TestProcessDueScheduledMessage_LogsUnifiedFields(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "proactive_log.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	user, _ := db.CreateUser("sched_log_owner", "Sched Log")
	botRow, _ := db.CreateBot(user.ID, "LogBot", "test", "wx_bot", json.RawMessage(`{}`))
	msg := &store.ScheduledMessage{BotID: botRow.ID, Sender: "wxid_a", FireAt: time.Now().Add(-30 * time.Minute).Unix(), Tolerance: 60, Prompt: "x", Origin: "promise", Status: "pending"}
	if err := db.AddScheduledMessage(msg); err != nil {
		t.Fatal(err)
	}

	var got string
	h := &captureSlogHandler{fn: func(rec slog.Record) {
		var result string
		rec.Attrs(func(a slog.Attr) bool {
			if a.Key == "result" {
				result = a.Value.String()
			}
			return true
		})
		if result != "" {
			got = result
		}
	}}
	old := slog.Default()
	slog.SetDefault(slog.New(h))
	defer slog.SetDefault(old)

	m := &Manager{store: db, aiSink: &sink.AI{}, instances: map[string]*Instance{botRow.ID: NewInstance(botRow.ID, &fakeProvider{})}}
	m.processDueScheduledMessage(msg, store.AIConfig{}, time.Now())

	if got != "cancelled" {
		t.Fatalf("logged result = %q, want cancelled", got)
	}
}

// TestParseProactiveOutcome_Defer covers the manager's interpretation of the
// composed text: send / 【不发】(with reason) / 【稍后 N 分钟】.
func TestParseProactiveOutcome_Defer(t *testing.T) {
	action, minutes, reason := parseProactiveOutcome("【稍后 10 分钟】")
	if action != proactiveDefer || minutes != 10 || reason != "" {
		t.Fatalf("defer parse = (%v, %d, %q)", action, minutes, reason)
	}
	action, minutes, reason = parseProactiveOutcome("【稍后】")
	if action != proactiveDefer || minutes != proactiveDeferDefaultMinutes {
		t.Fatalf("defer default = (%v, %d, %q)", action, minutes, reason)
	}
	action, _, reason = parseProactiveOutcome("【不发】聊过了")
	if action != proactiveDecline || reason == "" {
		t.Fatalf("decline parse = (%v, %q)", action, reason)
	}
	action, _, _ = parseProactiveOutcome("好的，我这就发")
	if action != proactiveSend {
		t.Fatalf("send parse = %v", action)
	}
}

// TestProcessDueScheduledMessage_SendsWhenComposeReturnsText covers the Task 7
// manager wiring: a promise message passes the gate, the composed text is sent
// via the provider and the row is marked sent.
func TestProcessDueScheduledMessage_SendsWhenComposeReturnsText(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "proactive_send.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	user, err := db.CreateUser("sched_send_owner", "Sched Send")
	if err != nil {
		t.Fatal(err)
	}
	botRow, err := db.CreateBot(user.ID, "SendBot", "test", "wx_bot", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	// 24h WeChat session window: a fresh inbound context token is required.
	if _, err := db.SaveMessage(&store.Message{
		BotID:        botRow.ID,
		Direction:    "inbound",
		FromUserID:   "wxid_a",
		ToUserID:     "wx_bot",
		ContextToken: "ctx-1",
		ItemList:     json.RawMessage(`[{"type":"text","text":"hi"}]`),
	}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]any{"role": "assistant", "content": "好的，我这就发"},
				"finish_reason": "stop",
			}},
		})
	}))
	defer srv.Close()

	for k, v := range map[string]string{
		"ai.api_key":         "test-key",
		"ai.base_url":        srv.URL,
		"ai.model":           "test-model",
		"ai.humanize_wechat": "false",
	} {
		if err := db.SetConfig(k, v); err != nil {
			t.Fatal(err)
		}
	}

	msg := &store.ScheduledMessage{
		BotID:     botRow.ID,
		Sender:    "wxid_a",
		FireAt:    time.Now().Add(-time.Minute).Unix(),
		Tolerance: 3600,
		Prompt:    "想你了",
		Origin:    "promise",
		Status:    "pending",
	}
	if err := db.AddScheduledMessage(msg); err != nil {
		t.Fatal(err)
	}

	fp := &fakeProvider{}
	m := &Manager{
		store:     db,
		aiSink:    &sink.AI{Store: db},
		instances: map[string]*Instance{botRow.ID: NewInstance(botRow.ID, fp)},
	}
	m.processDueScheduledMessage(msg, store.AIConfig{}, time.Now())

	all, err := db.ListScheduledMessages(botRow.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range all {
		if got.ID == msg.ID {
			if got.Status != "sent" {
				t.Fatalf("status = %q, want sent", got.Status)
			}
			if fp.sentText != "好的，我这就发" {
				t.Fatalf("provider text = %q, want the composed message", fp.sentText)
			}
			return
		}
	}
	t.Fatalf("scheduled message %s not found", msg.ID)
}

// TestProcessDueScheduledMessage_DefersWhenComposeReturnsDefer: the AI asks to
// wait 【稍后 10 分钟】 → the original row is cancelled with 稍后 and a new
// pending row fires in ~10 minutes with RescheduleCount bumped to 1.
func TestProcessDueScheduledMessage_DefersWhenComposeReturnsDefer(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "proactive_defer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	user, err := db.CreateUser("sched_defer_owner", "Sched Defer")
	if err != nil {
		t.Fatal(err)
	}
	botRow, err := db.CreateBot(user.ID, "DeferBot", "test", "wx_bot", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveMessage(&store.Message{
		BotID:        botRow.ID,
		Direction:    "inbound",
		FromUserID:   "wxid_a",
		ToUserID:     "wx_bot",
		ContextToken: "ctx-1",
		ItemList:     json.RawMessage(`[{"type":"text","text":"hi"}]`),
	}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]any{"role": "assistant", "content": "【稍后 10 分钟】"},
				"finish_reason": "stop",
			}},
		})
	}))
	defer srv.Close()

	for k, v := range map[string]string{
		"ai.api_key": "test-key", "ai.base_url": srv.URL, "ai.model": "test-model",
	} {
		if err := db.SetConfig(k, v); err != nil {
			t.Fatal(err)
		}
	}

	msg := &store.ScheduledMessage{
		BotID:     botRow.ID,
		Sender:    "wxid_a",
		FireAt:    time.Now().Add(-time.Minute).Unix(),
		Tolerance: 3600,
		Prompt:    "想你了",
		Origin:    "promise",
		Status:    "pending",
	}
	if err := db.AddScheduledMessage(msg); err != nil {
		t.Fatal(err)
	}

	m := &Manager{
		store:     db,
		aiSink:    &sink.AI{Store: db},
		instances: map[string]*Instance{botRow.ID: NewInstance(botRow.ID, &fakeProvider{})},
	}
	now := time.Now()
	m.processDueScheduledMessage(msg, store.AIConfig{}, now)

	all, err := db.ListScheduledMessages(botRow.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	var orig *store.ScheduledMessage
	var newPending *store.ScheduledMessage
	for i := range all {
		if all[i].ID == msg.ID {
			orig = &all[i]
		}
		if all[i].ID != msg.ID && all[i].Sender == "wxid_a" && all[i].Origin == "promise" && all[i].Status == "pending" {
			newPending = &all[i]
		}
	}
	if orig == nil {
		t.Fatalf("original message %s not found", msg.ID)
	}
	if orig.Status != "cancelled" {
		t.Fatalf("original status = %q, want cancelled", orig.Status)
	}
	if orig.FailReason != "稍后" {
		t.Fatalf("original fail_reason = %q, want 稍后", orig.FailReason)
	}
	if newPending == nil {
		t.Fatal("expected a new pending message after defer")
	}
	if newPending.RescheduleCount != 1 {
		t.Fatalf("new pending reschedule_count = %d, want 1", newPending.RescheduleCount)
	}
	wantFire := now.Add(10 * time.Minute).Unix()
	if newPending.FireAt != wantFire {
		t.Fatalf("new pending fire_at = %d, want %d", newPending.FireAt, wantFire)
	}
	if newPending.Prompt != msg.Prompt {
		t.Fatalf("new pending prompt = %q, want %q", newPending.Prompt, msg.Prompt)
	}
}

// proactiveFakeImageProvider 是本文件内使用的 persona.ImageProvider 假实现：
// 返回 PNG 魔数字节，让 gen_image 工具真正把"产物"交付给用户。
type proactiveFakeImageProvider struct{}

func (p *proactiveFakeImageProvider) Generate(ctx context.Context, prompt, negativePrompt string, opts persona.ImageOptions) ([]byte, error) {
	return []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 1}, nil
}

// TestProcessDueScheduledMessage_MarksSentWhenFallbackDeliveredOnComposeError
// 覆盖 Task 7 修复:媒体轮产物+兜底说明已直发、但续轮 LLM 失败时,ComposeScheduledMessage
// 必须返回 ComposeOutcomeDelivered,manager 必须把消息标记为 sent 而不是 failed
// (用户实际上已经收到了产品 + canned 说明)。
func TestProcessDueScheduledMessage_MarksSentWhenFallbackDeliveredOnComposeError(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "proactive_partial.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	user, err := db.CreateUser("sched_partial_owner", "Sched Partial")
	if err != nil {
		t.Fatal(err)
	}
	botRow, err := db.CreateBot(user.ID, "PartialBot", "test", "wx_bot", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	// 24h WeChat session window: a fresh inbound context token is required.
	if _, err := db.SaveMessage(&store.Message{
		BotID:        botRow.ID,
		Direction:    "inbound",
		FromUserID:   "wxid_a",
		ToUserID:     "wx_bot",
		ContextToken: "ctx-1",
		ItemList:     json.RawMessage(`[{"type":"text","text":"hi"}]`),
	}); err != nil {
		t.Fatal(err)
	}

	// Mock LLM: call1 → gen_image 工具调用;call2(续轮)→ HTTP 500。
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{
					"message": map[string]any{
						"role": "assistant",
						"tool_calls": []map[string]any{{
							"id":       "call_1",
							"type":     "function",
							"function": map[string]any{"name": "gen_image", "arguments": `{"prompt":"x"}`},
						}},
					},
					"finish_reason": "tool_calls",
				}},
			})
			return
		}
		http.Error(w, "upstream boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	for k, v := range map[string]string{
		"ai.api_key":           "test-key",
		"ai.base_url":          srv.URL,
		"ai.model":             "test-model",
		"ai.humanize_wechat":   "false",
		"ai.proactive_enabled": "true",
	} {
		if err := db.SetConfig(k, v); err != nil {
			t.Fatal(err)
		}
	}

	msg := &store.ScheduledMessage{
		BotID:     botRow.ID,
		Sender:    "wxid_a",
		FireAt:    time.Now().Add(-time.Minute).Unix(),
		Tolerance: 3600, // high so the tolerance check does not preempt
		Prompt:    "想你了",
		Origin:    "proactive",
		Status:    "pending",
	}
	if err := db.AddScheduledMessage(msg); err != nil {
		t.Fatal(err)
	}

	eng := persona.New(db, nil, nil)
	eng.SetImageProvider(&proactiveFakeImageProvider{})
	m := &Manager{
		store:     db,
		aiSink:    &sink.AI{Store: db, Persona: eng},
		instances: map[string]*Instance{botRow.ID: NewInstance(botRow.ID, &fakeProvider{})},
	}
	// Gate passes (ProactiveEnabled=true) so the flow reaches live composition:
	// the media round delivers the image + canned explanation, then the
	// continuation round fails → compose-partial must still mark the row sent.
	m.processDueScheduledMessage(msg, store.AIConfig{ProactiveEnabled: true}, time.Now())

	all, err := db.ListScheduledMessages(botRow.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range all {
		if got.ID == msg.ID {
			if got.Status != "sent" {
				t.Fatalf("status = %q, want sent (media+fallback delivered despite compose error)", got.Status)
			}
			return
		}
	}
	t.Fatalf("scheduled message %s not found", msg.ID)
}

// TestProcessDueScheduledMessage_PromiseSelfCheckDeclines 覆盖 promise 自查
// 拒绝路径端到端:promise 消息直通闸门, mock LLM 以纯文本轮返回 【不发】聊过了,
// manager 必须把该行取消并在 fail_reason 中记录 AI 给出的拒绝理由(聊过了),
// 而不是静默发送或丢弃。
func TestProcessDueScheduledMessage_PromiseSelfCheckDeclines(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "promise_decline.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	user, err := db.CreateUser("sched_decline_owner", "Sched Decline")
	if err != nil {
		t.Fatal(err)
	}
	botRow, err := db.CreateBot(user.ID, "DeclineBot", "test", "wx_bot", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	// 24h WeChat session window: a fresh inbound context token is required.
	if _, err := db.SaveMessage(&store.Message{
		BotID:        botRow.ID,
		Direction:    "inbound",
		FromUserID:   "wxid_a",
		ToUserID:     "wx_bot",
		ContextToken: "ctx-1",
		ItemList:     json.RawMessage(`[{"type":"text","text":"hi"}]`),
	}); err != nil {
		t.Fatal(err)
	}

	// Mock LLM: 纯文本轮直接拒绝并给出理由。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]any{"role": "assistant", "content": fmt.Sprintf("%s聊过了", sink.EmptyReplyMarker)},
				"finish_reason": "stop",
			}},
		})
	}))
	defer srv.Close()

	for k, v := range map[string]string{
		"ai.api_key":         "test-key",
		"ai.base_url":        srv.URL,
		"ai.model":           "test-model",
		"ai.humanize_wechat": "false",
	} {
		if err := db.SetConfig(k, v); err != nil {
			t.Fatal(err)
		}
	}

	msg := &store.ScheduledMessage{
		BotID:     botRow.ID,
		Sender:    "wxid_a",
		FireAt:    time.Now().Add(-time.Minute).Unix(),
		Tolerance: 3600,
		Prompt:    "想你了",
		Origin:    "promise",
		Status:    "pending",
	}
	if err := db.AddScheduledMessage(msg); err != nil {
		t.Fatal(err)
	}

	fp := &fakeProvider{}
	m := &Manager{
		store:     db,
		aiSink:    &sink.AI{Store: db},
		instances: map[string]*Instance{botRow.ID: NewInstance(botRow.ID, fp)},
	}
	m.processDueScheduledMessage(msg, store.AIConfig{}, time.Now())

	if len(fp.sentText) != 0 {
		t.Fatalf("provider text = %q, want nothing sent (AI declined with %s)", fp.sentText, sink.EmptyReplyMarker)
	}

	pending, err := db.ListPendingByScope(botRow.ID, "wxid_a", "promise")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending promise messages = %d, want 0 after decline: %+v", len(pending), pending)
	}

	all, err := db.ListScheduledMessages(botRow.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range all {
		if got.ID == msg.ID {
			if got.Status != "cancelled" {
				t.Fatalf("status = %q, want cancelled (AI declined with %s)", got.Status, sink.EmptyReplyMarker)
			}
			if !strings.Contains(got.FailReason, "聊过了") {
				t.Fatalf("fail_reason = %q, want it to record the decline reason 聊过了", got.FailReason)
			}
			return
		}
	}
	t.Fatalf("scheduled message %s not found", msg.ID)
}

// proactiveRecordingProvider 记录所有外发消息(含媒体 Data),用于断言产物直发
// 路径中图片与 canned 说明文本都真正到达了 provider(复用 fakeProvider 模式,
// 仅追加消息日志)。
type proactiveRecordingProvider struct {
	fakeProvider
	msgs []provider.OutboundMessage
}

func (p *proactiveRecordingProvider) Send(_ context.Context, msg provider.OutboundMessage) (string, error) {
	p.msgs = append(p.msgs, msg)
	return "client-1", nil
}

// TestProcessDueScheduledMessage_DeliveredOutcomeMarksSent 覆盖
// ComposeOutcomeDelivered 的成功分支(非错误路径):call1 返回 gen_image 工具
// (图片直发、canned 说明延后),call2(续轮)返回空文本(不是错误)→ runAIRound
// 补发 canned 说明并上报 Delivered → 即使最终文本为空,manager 也必须把消息
// 标记为 sent,且 provider 同时收到图片与说明文本。
func TestProcessDueScheduledMessage_DeliveredOutcomeMarksSent(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "proactive_delivered.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	user, err := db.CreateUser("sched_delivered_owner", "Sched Delivered")
	if err != nil {
		t.Fatal(err)
	}
	botRow, err := db.CreateBot(user.ID, "DeliveredBot", "test", "wx_bot", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	// 24h WeChat session window: a fresh inbound context token is required.
	if _, err := db.SaveMessage(&store.Message{
		BotID:        botRow.ID,
		Direction:    "inbound",
		FromUserID:   "wxid_a",
		ToUserID:     "wx_bot",
		ContextToken: "ctx-1",
		ItemList:     json.RawMessage(`[{"type":"text","text":"hi"}]`),
	}); err != nil {
		t.Fatal(err)
	}

	// Mock LLM: call1 → gen_image 工具调用;call2(续轮)→ 空文本(非错误)。
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{
					"message": map[string]any{
						"role": "assistant",
						"tool_calls": []map[string]any{{
							"id":       "call_1",
							"type":     "function",
							"function": map[string]any{"name": "gen_image", "arguments": `{"prompt":"x"}`},
						}},
					},
					"finish_reason": "tool_calls",
				}},
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]any{"role": "assistant", "content": ""},
				"finish_reason": "stop",
			}},
		})
	}))
	defer srv.Close()

	for k, v := range map[string]string{
		"ai.api_key":           "test-key",
		"ai.base_url":          srv.URL,
		"ai.model":             "test-model",
		"ai.humanize_wechat":   "false",
		"ai.proactive_enabled": "true",
	} {
		if err := db.SetConfig(k, v); err != nil {
			t.Fatal(err)
		}
	}

	msg := &store.ScheduledMessage{
		BotID:     botRow.ID,
		Sender:    "wxid_a",
		FireAt:    time.Now().Add(-time.Minute).Unix(),
		Tolerance: 3600, // high so the tolerance check does not preempt
		Prompt:    "想你了",
		Origin:    "proactive",
		Status:    "pending",
	}
	if err := db.AddScheduledMessage(msg); err != nil {
		t.Fatal(err)
	}

	eng := persona.New(db, nil, nil)
	eng.SetImageProvider(&proactiveFakeImageProvider{})
	fp := &proactiveRecordingProvider{}
	m := &Manager{
		store:     db,
		aiSink:    &sink.AI{Store: db, Persona: eng},
		instances: map[string]*Instance{botRow.ID: NewInstance(botRow.ID, fp)},
	}
	// Gate passes (ProactiveEnabled=true) so the flow reaches live composition.
	m.processDueScheduledMessage(msg, store.AIConfig{ProactiveEnabled: true}, time.Now())

	all, err := db.ListScheduledMessages(botRow.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, got := range all {
		if got.ID == msg.ID {
			found = true
			if got.Status != "sent" {
				t.Fatalf("status = %q, want sent (image + canned explanation delivered, zero final text)", got.Status)
			}
		}
	}
	if !found {
		t.Fatalf("scheduled message %s not found", msg.ID)
	}

	msgs := fp.msgs
	if len(msgs) != 2 {
		t.Fatalf("provider messages = %d, want 2 (image then canned explanation): %+v", len(msgs), msgs)
	}
	if msgs[0].Data == nil || msgs[0].FileName != "image.png" {
		t.Errorf("message 0 should be the delivered image, got %+v", msgs[0])
	}
	if !strings.Contains(msgs[1].Text, "我先把图发你") {
		t.Errorf("message 1 should be the flushed canned explanation, got %+v", msgs[1])
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("LLM api calls = %d, want exactly 2", got)
	}
}
