package sink

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openilink/openilink-hub/internal/provider"
	"github.com/openilink/openilink-hub/internal/store"
	"github.com/openilink/openilink-hub/internal/store/sqlite"
)

// recordingProvider records outbound texts for assertions.
type recordingProvider struct {
	mu               sync.Mutex
	sent             []provider.OutboundMessage
	failText         bool   // when true, text sends fail (media sends still succeed)
	failMedia        bool   // when true, media sends fail (text sends still succeed)
	failTextContains string // when set, only text sends containing this substring fail
}

func (p *recordingProvider) Name() string { return "mock" }
func (p *recordingProvider) Start(context.Context, provider.StartOptions) error {
	return nil
}
func (p *recordingProvider) Stop() {}
func (p *recordingProvider) Send(_ context.Context, msg provider.OutboundMessage) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if msg.Text != "" && (p.failText || (p.failTextContains != "" && strings.Contains(msg.Text, p.failTextContains))) {
		return "", errors.New("boom")
	}
	if msg.Text == "" && p.failMedia {
		return "", errors.New("media send failed")
	}
	p.sent = append(p.sent, msg)
	return "client_1", nil
}

func (p *recordingProvider) SendTyping(context.Context, string, string, bool) error { return nil }
func (p *recordingProvider) GetConfig(context.Context, string, string) (*provider.BotConfig, error) {
	return nil, nil
}
func (p *recordingProvider) DownloadMedia(context.Context, *provider.Media) ([]byte, error) {
	return nil, nil
}
func (p *recordingProvider) DownloadVoice(context.Context, *provider.Media, int) ([]byte, error) {
	return nil, nil
}
func (p *recordingProvider) Status() string { return "connected" }

func (p *recordingProvider) texts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.sent))
	for _, msg := range p.sent {
		if msg.Text != "" {
			out = append(out, msg.Text)
		}
	}
	return out
}

func (p *recordingProvider) messages() []provider.OutboundMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]provider.OutboundMessage{}, p.sent...)
}

func testSinkStore(t *testing.T) *sqlite.DB {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "sink.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestSendHumanized_SplitsWithPauses(t *testing.T) {
	p := &recordingProvider{}
	s := &AI{}
	ctx := context.Background()
	cfg := store.AIConfig{HumanizeWechat: true}

	start := time.Now()
	if err := s.SendHumanized(ctx, p, "wxid_a", "在呢\n刚看到消息\n怎么啦？", "", cfg); err != nil {
		t.Fatalf("SendHumanized: %v", err)
	}
	elapsed := time.Since(start)

	sent := p.texts()
	if len(sent) != 3 {
		t.Fatalf("sent parts = %d, want 3 (%v)", len(sent), sent)
	}
	if sent[0] != "在呢" || sent[1] != "刚看到消息" || sent[2] != "怎么啦？" {
		t.Errorf("parts mismatch: %v", sent)
	}
	// Pauses between parts should make the burst take > 0.
	if elapsed < 500*time.Millisecond {
		t.Errorf("expected pauses between parts, took %v", elapsed)
	}
}

func TestSendHumanized_DisabledSendsWhole(t *testing.T) {
	p := &recordingProvider{}
	s := &AI{}
	cfg := store.AIConfig{HumanizeWechat: false}
	if err := s.SendHumanized(context.Background(), p, "wxid_a", "a\nb\nc", "", cfg); err != nil {
		t.Fatal(err)
	}
	sent := p.texts()
	if len(sent) != 1 || sent[0] != "a\nb\nc" {
		t.Errorf("disabled humanize should send the whole text once, got %v", sent)
	}
}

func TestSendHumanized_CapsExcessiveParts(t *testing.T) {
	p := &recordingProvider{}
	s := &AI{}
	cfg := store.AIConfig{HumanizeWechat: true}
	var lines []string
	for i := 0; i < 12; i++ {
		lines = append(lines, "line"+strings.Repeat("x", 20))
	}
	if err := s.SendHumanized(context.Background(), p, "wxid_a", strings.Join(lines, "\n"), "", cfg); err != nil {
		t.Fatal(err)
	}
	sent := p.texts()
	if len(sent) > maxHumanizeParts {
		t.Errorf("parts = %d, must not exceed %d", len(sent), maxHumanizeParts)
	}
}

func TestSendHumanized_PreservesContextToken(t *testing.T) {
	p := &recordingProvider{}
	s := &AI{}
	cfg := store.AIConfig{HumanizeWechat: true}
	if err := s.SendHumanized(context.Background(), p, "wxid_a", "第一句\n第二句", "ctx-123", cfg); err != nil {
		t.Fatal(err)
	}
	for i, msg := range p.messages() {
		if msg.ContextToken != "ctx-123" {
			t.Fatalf("message %d context token = %q, want ctx-123", i, msg.ContextToken)
		}
	}
}

func TestHandleCommand_ReplyUsesContextToken(t *testing.T) {
	db := testSinkStore(t)
	if err := db.SetConfig("ai.available_models", `["gpt-4o-mini"]`); err != nil {
		t.Fatal(err)
	}

	p := &recordingProvider{}
	s := &AI{Store: db}
	d := Delivery{
		BotDBID:   "bot1",
		Provider:  p,
		Message:   provider.InboundMessage{Sender: "wxid_a", ContextToken: "ctx-cmd"},
		MsgType:   "text",
		Content:   "/model",
		AIEnabled: true,
	}
	s.Handle(d)

	msgs := p.messages()
	if len(msgs) == 0 {
		t.Fatal("expected command reply to be sent")
	}
	if msgs[0].ContextToken != "ctx-cmd" {
		t.Fatalf("command reply context token = %q, want ctx-cmd", msgs[0].ContextToken)
	}
}

// TestReplyThinkingNeverSent asserts design §11.1: even with
// hide_thinking=false, thinking content must never reach the provider.
func TestReplyThinkingNeverSent(t *testing.T) {
	db := testSinkStore(t)

	// Mock AI endpoint: returns a normal reply plus "thinking".
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"role":     "assistant",
						"content":  "你好呀",
						"thinking": "这是内心独白，绝不能发出去",
					},
					"finish_reason": "stop",
				},
			},
		})
	}))
	defer srv.Close()

	// AI config with hide_thinking = false (the old leak path).
	for k, v := range map[string]string{
		"ai.api_key":         "test-key",
		"ai.base_url":        srv.URL,
		"ai.model":           "test-model",
		"ai.hide_thinking":   "false",
		"ai.humanize_wechat": "false",
	} {
		if err := db.SetConfig(k, v); err != nil {
			t.Fatal(err)
		}
	}

	p := &recordingProvider{}
	s := &AI{Store: db}
	d := Delivery{
		BotDBID:   "bot1",
		Provider:  p,
		Message:   provider.InboundMessage{Sender: "wxid_a"},
		SeqID:     1,
		MsgType:   "text",
		Content:   "hi",
		AIEnabled: true,
	}
	s.reply(d)

	sent := p.texts()
	if len(sent) == 0 {
		t.Fatal("expected a reply to be sent")
	}
	for _, text := range sent {
		if strings.Contains(text, "内心独白") || strings.Contains(text, "💭") {
			t.Errorf("thinking leaked to user: %q", text)
		}
	}
	// The real content is still delivered.
	if !strings.Contains(strings.Join(sent, ""), "你好呀") {
		t.Errorf("reply content missing: %v", sent)
	}
}
