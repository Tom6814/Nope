package sink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	aiinternal "github.com/openilink/openilink-hub/internal/ai"
	"github.com/openilink/openilink-hub/internal/persona"
	"github.com/openilink/openilink-hub/internal/provider"
	"github.com/openilink/openilink-hub/internal/store"
)

// fakeProactiveSearcher satisfies persona.Searcher for tests: a single fixed
// hit so web_search produces a deterministic text conclusion.
type fakeProactiveSearcher struct {
	results []persona.SearchResult
}

func (f fakeProactiveSearcher) Search(_ context.Context, _ string) ([]persona.SearchResult, error) {
	return f.results, nil
}

// fakeProactiveImage satisfies persona.ImageProvider for tests: returns PNG
// magic bytes so gen_image produces a deliverable image without a backend.
type fakeProactiveImage struct {
	images [][]byte
	calls  int
}

func (f *fakeProactiveImage) Generate(_ context.Context, _, _ string, _ persona.ImageOptions) ([]byte, error) {
	if f.calls >= len(f.images) {
		return nil, errors.New("no fake image prepared")
	}
	img := append([]byte(nil), f.images[f.calls]...)
	f.calls++
	return img, nil
}

// fakeCompleter satisfies persona.Completer for constructing a persona engine
// in unit tests (never invoked by the delivery layer).
var fakeCompleter = func(ctx context.Context, cfg store.AIConfig, msgs []aiinternal.Message, tools []aiinternal.Tool) (*aiinternal.CompletionResult, error) {
	return &aiinternal.CompletionResult{Content: ""}, nil
}

func TestParseCustomHeaders_ObjectFormat(t *testing.T) {
	m := parseCustomHeaders(`{"HTTP-Referer":"https://openclaw.ai","X-Title":"OpenClaw"}`)
	if m == nil {
		t.Fatal("expected non-nil map")
	}
	if m["HTTP-Referer"] != "https://openclaw.ai" {
		t.Errorf("HTTP-Referer = %q", m["HTTP-Referer"])
	}
	if m["X-Title"] != "OpenClaw" {
		t.Errorf("X-Title = %q", m["X-Title"])
	}
}

func TestParseCustomHeaders_ArrayFormat(t *testing.T) {
	m := parseCustomHeaders(`[["HTTP-Referer","https://openclaw.ai"],["X-Title","OpenClaw"]]`)
	if m == nil {
		t.Fatal("expected non-nil map")
	}
	if m["HTTP-Referer"] != "https://openclaw.ai" {
		t.Errorf("HTTP-Referer = %q", m["HTTP-Referer"])
	}
}

func TestParseCustomHeaders_EmptyKeyFiltered(t *testing.T) {
	m := parseCustomHeaders(`[["","value"],["X-Good","ok"]]`)
	if _, ok := m[""]; ok {
		t.Error("empty key should be filtered")
	}
	if m["X-Good"] != "ok" {
		t.Errorf("X-Good = %q", m["X-Good"])
	}
}

func TestParseCustomHeaders_InvalidJSON(t *testing.T) {
	m := parseCustomHeaders(`not json`)
	if m != nil {
		t.Errorf("expected nil for invalid JSON, got %v", m)
	}
}

func TestParseCustomHeaders_Empty(t *testing.T) {
	m := parseCustomHeaders(`{}`)
	if m != nil {
		t.Errorf("expected nil for empty object, got %v", m)
	}
	m = parseCustomHeaders(`[]`)
	if m != nil {
		t.Errorf("expected nil for empty array, got %v", m)
	}
}

func TestToolResultsForContinuation_StripsMediaButPreservesMetadata(t *testing.T) {
	original := []aiinternal.ToolCallResult{
		{
			ID:      "call_1",
			Name:    "write_doc",
			Content: "document created",
			Images: []aiinternal.ImageData{
				{Data: []byte("image"), ContentType: "image/png"},
			},
			Files: []aiinternal.FileData{
				{FileName: "report.md", Data: []byte("# report")},
			},
			Delivery: aiinternal.ToolCallDelivery{
				UserFacing:  true,
				Explanation: "文档已生成",
			},
			Diagnostics: aiinternal.ToolCallDiagnostics{
				ErrorStage:   "deliver",
				ErrorMessage: "network timeout",
				LogFields:    map[string]string{"delivery_kind": "file"},
			},
		},
		{
			ID:      "call_2",
			Name:    "web_search",
			Content: "sunny",
			Delivery: aiinternal.ToolCallDelivery{
				UserFacing:  true,
				Explanation: "已经查询天气",
			},
			Diagnostics: aiinternal.ToolCallDiagnostics{
				LogFields: map[string]string{"source": "search"},
			},
		},
	}

	got := toolResultsForContinuation(original)

	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if len(got[0].Images) != 0 {
		t.Fatalf("continuation images = %d, want 0", len(got[0].Images))
	}
	if len(got[0].Files) != 0 {
		t.Fatalf("continuation files = %d, want 0", len(got[0].Files))
	}
	if got[0].Delivery != original[0].Delivery {
		t.Fatal("delivery metadata should be preserved")
	}
	if got[0].Diagnostics.ErrorStage != original[0].Diagnostics.ErrorStage {
		t.Fatalf("error stage = %q, want %q", got[0].Diagnostics.ErrorStage, original[0].Diagnostics.ErrorStage)
	}
	if got[0].Diagnostics.ErrorMessage != original[0].Diagnostics.ErrorMessage {
		t.Fatalf("error message = %q, want %q", got[0].Diagnostics.ErrorMessage, original[0].Diagnostics.ErrorMessage)
	}
	if got[0].Diagnostics.LogFields["delivery_kind"] != "file" {
		t.Fatal("diagnostic log fields should be preserved")
	}
	if len(got[1].Images) != 0 || len(got[1].Files) != 0 {
		t.Fatal("result without media should remain without media")
	}
	if got[1].Delivery != original[1].Delivery {
		t.Fatal("delivery metadata should be preserved for plain results")
	}
	if got[1].Diagnostics.LogFields["source"] != "search" {
		t.Fatal("plain result diagnostics should be preserved")
	}
	if len(original[0].Images) != 1 {
		t.Fatalf("original images mutated to %d, want 1", len(original[0].Images))
	}
	if len(original[0].Files) != 1 {
		t.Fatalf("original files mutated to %d, want 1", len(original[0].Files))
	}
}

func TestDeliverToolProducts_DefersFileExplanation(t *testing.T) {
	db := testSinkStore(t)
	eng := persona.New(db, nil, fakeCompleter)
	s := &AI{Store: db, Persona: eng}
	p := &recordingProvider{}
	d := Delivery{
		BotDBID:  "bot1",
		Provider: p,
		Message:  provider.InboundMessage{Sender: "wxid_a", ContextToken: "ctx-1"},
	}

	allDelivered, pending := s.deliverToolProducts(context.Background(), d, []aiinternal.ToolCallResult{{
		Name:        "write_doc",
		Files:       []aiinternal.FileData{{FileName: "旅行计划.md", ContentType: "text/markdown", Data: []byte("# hi")}},
		Delivery:    aiinternal.ToolCallDelivery{UserFacing: true, Explanation: "我整理好了，你直接看这个文档就行。"},
		Diagnostics: aiinternal.ToolCallDiagnostics{LogFields: map[string]string{"delivery_kind": "file"}},
	}})
	if !allDelivered {
		t.Fatal("expected deliverToolProducts to report successful delivery")
	}

	// Media rounds must NOT send the canned explanation immediately: only the
	// file goes out, the explanation is deferred to pending for LLM refinement.
	msgs := p.messages()
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1 (file only, explanation deferred) (%+v)", len(msgs), msgs)
	}
	if msgs[0].Data == nil || msgs[0].FileName != "旅行计划.md" {
		t.Errorf("message 0 should be the file, got %+v", msgs[0])
	}
	if len(pending) != 1 || pending[0] != "我整理好了，你直接看这个文档就行。" {
		t.Errorf("pending explanations = %#v, want the canned sentence deferred", pending)
	}
}

func TestDeliverToolProducts_DefersImageExplanation(t *testing.T) {
	db := testSinkStore(t)
	eng := persona.New(db, nil, fakeCompleter)
	s := &AI{Store: db, Persona: eng}
	p := &recordingProvider{}
	d := Delivery{
		BotDBID:  "bot1",
		Provider: p,
		Message:  provider.InboundMessage{Sender: "wxid_a", ContextToken: "ctx-1"},
	}

	allDelivered, pending := s.deliverToolProducts(context.Background(), d, []aiinternal.ToolCallResult{{
		Name:        "gen_image",
		Images:      []aiinternal.ImageData{{Data: []byte{0x89, 'P', 'N', 'G'}, ContentType: "image/png"}},
		Delivery:    aiinternal.ToolCallDelivery{UserFacing: true, Explanation: "我先把图发你，你看看是不是这个感觉。"},
		Diagnostics: aiinternal.ToolCallDiagnostics{LogFields: map[string]string{"delivery_kind": "image"}},
	}})
	if !allDelivered {
		t.Fatal("expected deliverToolProducts to report successful delivery")
	}

	// Media rounds must NOT send the canned explanation immediately: only the
	// image goes out, the explanation is deferred to pending for LLM refinement.
	msgs := p.messages()
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1 (image only, explanation deferred) (%+v)", len(msgs), msgs)
	}
	if msgs[0].Data == nil || msgs[0].FileName != "image.png" {
		t.Errorf("message 0 should be the image, got %+v", msgs[0])
	}
	if len(pending) != 1 || pending[0] != "我先把图发你，你看看是不是这个感觉。" {
		t.Errorf("pending explanations = %#v, want the canned sentence deferred", pending)
	}
}

func TestDeliverToolProducts_TextOnlySendsExplanationDirectly(t *testing.T) {
	db := testSinkStore(t)
	eng := persona.New(db, nil, fakeCompleter)
	s := &AI{Store: db, Persona: eng}
	p := &recordingProvider{}
	d := Delivery{
		BotDBID:  "bot1",
		Provider: p,
		Message:  provider.InboundMessage{Sender: "wxid_a", ContextToken: "ctx-1"},
	}

	ok, _ := s.deliverToolProducts(context.Background(), d, []aiinternal.ToolCallResult{{
		Name:        "web_search",
		Delivery:    aiinternal.ToolCallDelivery{UserFacing: true, Explanation: "我刚查了下，杭州今天有阵雨。"},
		Diagnostics: aiinternal.ToolCallDiagnostics{LogFields: map[string]string{"delivery_kind": "text"}},
	}})
	if !ok {
		t.Fatal("expected deliverToolProducts to report successful delivery")
	}

	msgs := p.messages()
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1 (%+v)", len(msgs), msgs)
	}
	if msgs[0].Text != "我刚查了下，杭州今天有阵雨。" {
		t.Errorf("message text = %q, want explanation", msgs[0].Text)
	}
	if msgs[0].Data != nil {
		t.Errorf("no media expected, got %+v", msgs[0])
	}
}

func TestDeliverToolProducts_IgnoresNonPersonaResults(t *testing.T) {
	db := testSinkStore(t)
	eng := persona.New(db, nil, fakeCompleter)
	s := &AI{Store: db, Persona: eng}
	p := &recordingProvider{}
	d := Delivery{
		BotDBID:  "bot1",
		Provider: p,
		Message:  provider.InboundMessage{Sender: "wxid_a", ContextToken: "ctx-1"},
	}

	if ok, _ := s.deliverToolProducts(context.Background(), d, []aiinternal.ToolCallResult{{
		Name:     "someapp__tool",
		Files:    []aiinternal.FileData{{FileName: "x.txt", Data: []byte("x")}},
		Delivery: aiinternal.ToolCallDelivery{UserFacing: true, Explanation: "不应重发"},
	}}); !ok {
		t.Fatal("non-persona results must not affect delivery success")
	}

	if msgs := p.messages(); len(msgs) != 0 {
		t.Fatalf("non-persona results must not be re-delivered, got %+v", msgs)
	}
}

func TestDeliverToolProducts_SavesExplanationToHistory(t *testing.T) {
	db := testSinkStore(t)
	eng := persona.New(db, nil, fakeCompleter)
	s := &AI{Store: db, Persona: eng}
	p := &recordingProvider{}
	d := Delivery{
		BotDBID:  "bot1",
		Provider: p,
		Message:  provider.InboundMessage{Sender: "wxid_a", ContextToken: "ctx-1"},
	}

	// Text-only rounds deliver the explanation immediately and persist it to
	// history (media-round explanations are deferred to flushPendingExplanations).
	const explanation = "我刚查了下，杭州今天有阵雨。"
	ok, _ := s.deliverToolProducts(context.Background(), d, []aiinternal.ToolCallResult{{
		Name:        "web_search",
		Delivery:    aiinternal.ToolCallDelivery{UserFacing: true, Explanation: explanation},
		Diagnostics: aiinternal.ToolCallDiagnostics{LogFields: map[string]string{"delivery_kind": "text"}},
	}})
	if !ok {
		t.Fatal("expected deliverToolProducts to report successful delivery")
	}

	history, err := db.ListMessagesBySender("bot1", "wxid_a", 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range history {
		if m.Direction == "outbound" && strings.Contains(string(m.ItemList), explanation) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("explanation not saved to message history: %+v", history)
	}
}

func TestDeliverToolProducts_ReturnsFalseWhenExplanationSendFails(t *testing.T) {
	db := testSinkStore(t)
	eng := persona.New(db, nil, fakeCompleter)
	s := &AI{Store: db, Persona: eng}
	p := &recordingProvider{failText: true}
	d := Delivery{
		BotDBID:  "bot1",
		Provider: p,
		Message:  provider.InboundMessage{Sender: "wxid_a", ContextToken: "ctx-1"},
	}

	const explanation = "我刚查了下，杭州今天有阵雨。"
	ok, _ := s.deliverToolProducts(context.Background(), d, []aiinternal.ToolCallResult{{
		Name:        "web_search",
		Delivery:    aiinternal.ToolCallDelivery{UserFacing: true, Explanation: explanation},
		Diagnostics: aiinternal.ToolCallDiagnostics{LogFields: map[string]string{"delivery_kind": "text"}},
	}})
	if ok {
		t.Fatal("expected deliverToolProducts to report failure when explanation send fails")
	}

	history, err := db.ListMessagesBySender("bot1", "wxid_a", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range history {
		if strings.Contains(string(m.ItemList), explanation) {
			t.Fatalf("failed explanation must not be saved to history: %+v", history)
		}
	}
}

func TestDeliverToolProducts_ReturnsFalseWhenMediaSendFails(t *testing.T) {
	db := testSinkStore(t)
	eng := persona.New(db, nil, fakeCompleter)
	s := &AI{Store: db, Persona: eng}
	p := &recordingProvider{failMedia: true}
	d := Delivery{
		BotDBID:  "bot1",
		Provider: p,
		Message:  provider.InboundMessage{Sender: "wxid_a", ContextToken: "ctx-1"},
	}

	ok, _ := s.deliverToolProducts(context.Background(), d, []aiinternal.ToolCallResult{{
		Name:        "gen_image",
		Images:      []aiinternal.ImageData{{Data: []byte{0x89, 'P', 'N', 'G'}, ContentType: "image/png"}},
		Delivery:    aiinternal.ToolCallDelivery{UserFacing: true, Explanation: "我先把图发你，你看看是不是这个感觉。"},
		Diagnostics: aiinternal.ToolCallDiagnostics{LogFields: map[string]string{"delivery_kind": "image"}},
	}})
	if ok {
		t.Fatal("expected deliverToolProducts to report failure when media send fails")
	}

	// The explanation is deferred (not sent) on media rounds, so nothing reaches
	// the provider when the media send itself fails.
	if msgs := p.messages(); len(msgs) != 0 {
		t.Fatalf("messages = %d, want none (media failed, explanation deferred) (%+v)", len(msgs), msgs)
	}
}

func TestDeliverToolProducts_ReturnsFalseWhenFileSendFails(t *testing.T) {
	db := testSinkStore(t)
	eng := persona.New(db, nil, fakeCompleter)
	s := &AI{Store: db, Persona: eng}
	p := &recordingProvider{failMedia: true}
	d := Delivery{
		BotDBID:  "bot1",
		Provider: p,
		Message:  provider.InboundMessage{Sender: "wxid_a", ContextToken: "ctx-1"},
	}

	ok, _ := s.deliverToolProducts(context.Background(), d, []aiinternal.ToolCallResult{{
		Name:        "write_doc",
		Files:       []aiinternal.FileData{{FileName: "旅行计划.md", ContentType: "text/markdown", Data: []byte("# hi")}},
		Delivery:    aiinternal.ToolCallDelivery{UserFacing: true, Explanation: "我整理好了，你直接看这个文档就行。"},
		Diagnostics: aiinternal.ToolCallDiagnostics{LogFields: map[string]string{"delivery_kind": "file"}},
	}})
	if ok {
		t.Fatal("expected deliverToolProducts to report failure when file send fails")
	}

	// The explanation is deferred (not sent) on media rounds, so nothing reaches
	// the provider when the file send itself fails.
	if msgs := p.messages(); len(msgs) != 0 {
		t.Fatalf("messages = %d, want none (file failed, explanation deferred) (%+v)", len(msgs), msgs)
	}
}

func TestFlushPendingExplanations_SendsAndSaves(t *testing.T) {
	db := testSinkStore(t)
	eng := persona.New(db, nil, fakeCompleter)
	s := &AI{Store: db, Persona: eng}
	p := &recordingProvider{}
	d := Delivery{
		BotDBID:  "bot1",
		Provider: p,
		Message:  provider.InboundMessage{Sender: "wxid_a", ContextToken: "ctx-1"},
	}

	const explanation = "我先把图发你，你看看是不是这个感觉。"
	s.flushPendingExplanations(context.Background(), d, []string{explanation})

	msgs := p.messages()
	if len(msgs) != 1 || msgs[0].Text != explanation {
		t.Fatalf("expected the fallback explanation sent, got %+v", msgs)
	}

	history, err := db.ListMessagesBySender("bot1", "wxid_a", 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range history {
		if m.Direction == "outbound" && strings.Contains(string(m.ItemList), "我先把图发你") {
			found = true
		}
	}
	if !found {
		t.Fatalf("flushed explanation not saved to history: %+v", history)
	}
}

func TestShouldSkipToolLLM(t *testing.T) {
	db := testSinkStore(t)
	eng := persona.New(db, nil, fakeCompleter)
	s := &AI{Store: db, Persona: eng}

	cases := []struct {
		name    string
		results []aiinternal.ToolCallResult
		want    bool
	}{
		{"all async", []aiinternal.ToolCallResult{{Name: "someapp__tool", Async: true}}, true},
		{"web_search text-only", []aiinternal.ToolCallResult{{Name: "web_search", Delivery: aiinternal.ToolCallDelivery{UserFacing: true, Explanation: "x"}}}, true},
		{"write_doc with files + explanation", []aiinternal.ToolCallResult{{Name: "write_doc", Delivery: aiinternal.ToolCallDelivery{UserFacing: true, Explanation: "x"}, Files: []aiinternal.FileData{{FileName: "a.md", Data: []byte("x")}}}}, false},
		{"gen_image with images + explanation", []aiinternal.ToolCallResult{{Name: "gen_image", Delivery: aiinternal.ToolCallDelivery{UserFacing: true, Explanation: "x"}, Images: []aiinternal.ImageData{{Data: []byte("x")}}}}, false},
		{"user-facing empty explanation", []aiinternal.ToolCallResult{{Name: "web_search", Delivery: aiinternal.ToolCallDelivery{UserFacing: true}}}, false},
		{"empty", nil, true},
		{"app tool not async", []aiinternal.ToolCallResult{{Name: "someapp__tool", Delivery: aiinternal.ToolCallDelivery{UserFacing: true}}}, false},
		{"mixed persona text + async app", []aiinternal.ToolCallResult{
			{Name: "web_search", Delivery: aiinternal.ToolCallDelivery{UserFacing: true, Explanation: "x"}},
			{Name: "someapp__tool", Async: true},
		}, true},
		{"mixed persona text + app non-async", []aiinternal.ToolCallResult{
			{Name: "web_search", Delivery: aiinternal.ToolCallDelivery{UserFacing: true, Explanation: "x"}},
			{Name: "someapp__tool", Delivery: aiinternal.ToolCallDelivery{UserFacing: true}},
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.shouldSkipToolLLM(tc.results); got != tc.want {
				t.Errorf("shouldSkipToolLLM = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestReply_PersonaToolProductsDeliveredDirectly asserts the full reply()
// chain for a persona media round (design §11.2 + §6.3): the LLM returns a
// write_doc tool call, the persona engine executes it in-process, the product
// file is delivered directly, and the media round continues with an LLM
// refinement round whose text is then sent to the user — so the mock LLM
// endpoint is called exactly twice and the canned explanation is never sent.
func TestReply_PersonaToolProductsDeliveredDirectly(t *testing.T) {
	db := testSinkStore(t)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			// First call: the LLM decides to write a doc.
			json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{
					{
						"message": map[string]any{
							"role": "assistant",
							"tool_calls": []map[string]any{
								{
									"id":       "call_1",
									"type":     "function",
									"function": map[string]any{"name": "write_doc", "arguments": `{"title":"旅行计划","content":"# hi"}`},
								},
							},
						},
						"finish_reason": "tool_calls",
					},
				},
			})
			return
		}
		// Second call: the continuation round refines the explanation.
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"message":       map[string]any{"role": "assistant", "content": "文档给你整理好了，直接看这个就行。"},
					"finish_reason": "stop",
				},
			},
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

	eng := persona.New(db, nil, fakeCompleter)
	s := &AI{Store: db, Persona: eng}
	p := &recordingProvider{}
	d := Delivery{
		BotDBID:   "bot1",
		Provider:  p,
		Message:   provider.InboundMessage{Sender: "wxid_a"},
		SeqID:     1,
		MsgType:   "text",
		Content:   "帮我写个旅行计划",
		AIEnabled: true,
	}
	s.reply(d)

	msgs := p.messages()
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2 (file then LLM-refined explanation): %+v", len(msgs), msgs)
	}
	if msgs[0].Data == nil || msgs[0].FileName != "旅行计划.md" {
		t.Errorf("message 0 should be the file, got %+v", msgs[0])
	}
	if msgs[1].Text != "文档给你整理好了，直接看这个就行。" {
		t.Errorf("message 1 should be the LLM-refined explanation, got %+v", msgs[1])
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("LLM api calls = %d, want exactly 2 (media round must continue with the LLM refinement)", got)
	}
}

func TestComposeReschedule_ReturnsNewTime(t *testing.T) {
	// 用计算出的未来时间而非硬编码日期，避免沙箱时钟漂移导致断言不稳定。
	future := time.Now().Add(24 * time.Hour).Format("2006-01-02 15:04")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]any{"role": "assistant", "content": future},
				"finish_reason": "stop",
			}},
		})
	}))
	defer srv.Close()
	db := testSinkStore(t)
	for k, v := range map[string]string{"ai.api_key": "test-key", "ai.base_url": srv.URL, "ai.model": "test-model"} {
		if err := db.SetConfig(k, v); err != nil {
			t.Fatal(err)
		}
	}
	s := &AI{Store: db}
	ts, ok, err := s.ComposeReschedule(context.Background(), "bot1", "wxid_a", "想你了", "静默时段 23:00-08:00")
	if err != nil || !ok {
		t.Fatalf("ComposeReschedule = (%d, %v, %v), want ok", ts, ok, err)
	}
	if ts <= time.Now().Unix() {
		t.Fatalf("rescheduled fire_at %d should be in the future", ts)
	}
}

func TestComposeReschedule_Declines(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]any{"role": "assistant", "content": "【不发】"},
				"finish_reason": "stop",
			}},
		})
	}))
	defer srv.Close()
	db := testSinkStore(t)
	for k, v := range map[string]string{"ai.api_key": "test-key", "ai.base_url": srv.URL, "ai.model": "test-model"} {
		if err := db.SetConfig(k, v); err != nil {
			t.Fatal(err)
		}
	}
	s := &AI{Store: db}
	_, ok, err := s.ComposeReschedule(context.Background(), "bot1", "wxid_a", "想你了", "静默时段")
	if err != nil {
		t.Fatalf("ComposeReschedule: %v", err)
	}
	if ok {
		t.Fatal("expected decline when AI returns 【不发】")
	}
}

// TestComposeReschedule_ParsesTimeWithChatter reproduces Task 6 finding:
// ComposeReschedule feeds the WHOLE AI reply to persona.ParseTime, but
// time.Parse requires the entire string to match — a chatty reply like
// "那就明天上午10点吧 2026-08-13 10:00" fails to parse and is silently
// treated as a decline (the scheduled message is permanently lost). The fix
// must extract the first YYYY-MM-DD[ T]HH:MM substring and parse that.
func TestComposeReschedule_ParsesTimeWithChatter(t *testing.T) {
	// 用计算出的未来时间而非硬编码日期，避免沙箱时钟漂移导致断言不稳定。
	future := time.Now().Add(48 * time.Hour).Format("2006-01-02 15:04")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]any{"role": "assistant", "content": fmt.Sprintf("那就明天上午10点吧 %s", future)},
				"finish_reason": "stop",
			}},
		})
	}))
	defer srv.Close()
	db := testSinkStore(t)
	for k, v := range map[string]string{"ai.api_key": "test-key", "ai.base_url": srv.URL, "ai.model": "test-model"} {
		if err := db.SetConfig(k, v); err != nil {
			t.Fatal(err)
		}
	}
	s := &AI{Store: db}
	ts, ok, err := s.ComposeReschedule(context.Background(), "bot1", "wxid_a", "想你了", "静默时段")
	if err != nil || !ok {
		t.Fatalf("ComposeReschedule = (%d, %v, %v), want ok", ts, ok, err)
	}
}

// TestComposeScheduledMessage_GeneratesImageWithRefinedExplanation locks in
// the Task 7 upgrade: ComposeScheduledMessage runs the full runAIRound with
// persona tools, so a gen_image round delivers the image directly and then the
// LLM continuation produces the refined explanation that the manager sends.
func TestComposeScheduledMessage_GeneratesImageWithRefinedExplanation(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			// First call: the LLM decides to draw an image.
			json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{
					"message": map[string]any{
						"role": "assistant",
						"tool_calls": []map[string]any{{
							"id":       "call_1",
							"type":     "function",
							"function": map[string]any{"name": "gen_image", "arguments": `{"prompt":"雨后彩虹"}`},
						}},
					},
					"finish_reason": "tool_calls",
				}},
			})
			return
		}
		// Second call: the continuation round refines the explanation.
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]any{"role": "assistant", "content": "刚画了一张，你看看这个感觉对不对"},
				"finish_reason": "stop",
			}},
		})
	}))
	defer srv.Close()

	db := testSinkStore(t)
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
	eng := persona.New(db, nil, fakeCompleter)
	eng.SetImageProvider(&fakeProactiveImage{
		images: [][]byte{{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 1}},
	})
	s := &AI{Store: db, Persona: eng}
	p := &recordingProvider{}

	text, outcome, err := s.ComposeScheduledMessage(context.Background(), "bot1", "wxid_a", "想给你画张彩虹", "你们刚聊过", p)
	if err != nil {
		t.Fatalf("ComposeScheduledMessage: %v", err)
	}
	if outcome != ComposeOutcomeText {
		t.Fatalf("outcome = %v, want ComposeOutcomeText", outcome)
	}
	if !strings.Contains(text, "刚画了一张") {
		t.Fatalf("final text = %q, want the refined explanation", text)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("LLM calls = %d, want 2 (tool round + refined explanation)", got)
	}
	msgs := p.messages()
	if len(msgs) < 1 || msgs[0].Data == nil {
		t.Fatalf("expected the generated image delivered first, got %+v", msgs)
	}
}

// TestComposeScheduledMessage_Declines: a text-only round returns the decline
// marker as ComposeOutcomeText — the MANAGER parses 【不发】. ComposeScheduledMessage
// itself must not swallow it.
func TestComposeScheduledMessage_Declines(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]any{"role": "assistant", "content": "【不发】刚聊过这事了"},
				"finish_reason": "stop",
			}},
		})
	}))
	defer srv.Close()
	db := testSinkStore(t)
	for k, v := range map[string]string{
		"ai.api_key": "test-key", "ai.base_url": srv.URL, "ai.model": "test-model",
	} {
		if err := db.SetConfig(k, v); err != nil {
			t.Fatal(err)
		}
	}
	s := &AI{Store: db}
	p := &recordingProvider{}
	text, outcome, err := s.ComposeScheduledMessage(context.Background(), "bot1", "wxid_a", "想你了", "", p)
	if err != nil {
		t.Fatalf("ComposeScheduledMessage: %v", err)
	}
	if outcome != ComposeOutcomeText {
		t.Fatalf("outcome = %v, want ComposeOutcomeText (the manager parses the marker)", outcome)
	}
	if !strings.Contains(text, EmptyReplyMarker) {
		t.Fatalf("text = %q, want it to carry %s for the manager to decline", text, EmptyReplyMarker)
	}
}

// TestComposeScheduledMessage_Empty: AI returns nothing and no tool delivered
// anything → ComposeOutcomeEmpty.
func TestComposeScheduledMessage_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]any{"role": "assistant", "content": ""},
				"finish_reason": "stop",
			}},
		})
	}))
	defer srv.Close()
	db := testSinkStore(t)
	for k, v := range map[string]string{
		"ai.api_key": "test-key", "ai.base_url": srv.URL, "ai.model": "test-model",
	} {
		if err := db.SetConfig(k, v); err != nil {
			t.Fatal(err)
		}
	}
	s := &AI{Store: db}
	p := &recordingProvider{}
	text, outcome, err := s.ComposeScheduledMessage(context.Background(), "bot1", "wxid_a", "想你了", "", p)
	if err != nil {
		t.Fatalf("ComposeScheduledMessage: %v", err)
	}
	if outcome != ComposeOutcomeEmpty {
		t.Fatalf("outcome = %v, want ComposeOutcomeEmpty", outcome)
	}
	if text != "" {
		t.Fatalf("text = %q, want empty", text)
	}
}

func TestRunAIRound_TextRoundReturnsOutcomeText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]any{"role": "assistant", "content": "你好呀"},
				"finish_reason": "stop",
			}},
		})
	}))
	defer srv.Close()
	db := testSinkStore(t)
	for k, v := range map[string]string{
		"ai.api_key": "test-key", "ai.base_url": srv.URL, "ai.model": "test-model",
	} {
		if err := db.SetConfig(k, v); err != nil {
			t.Fatal(err)
		}
	}
	cfg := store.AIConfig{APIKey: "test-key", BaseURL: srv.URL, Model: "test-model"}
	s := &AI{Store: db}
	msgs := []aiinternal.Message{{Role: "user", Content: "hi"}}
	res, err := s.runAIRound(context.Background(), Delivery{}, cfg, msgs, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("runAIRound: %v", err)
	}
	if res.Text != "你好呀" {
		t.Fatalf("text = %q, want 你好呀", res.Text)
	}
	if res.Thinking != "" {
		t.Fatalf("thinking = %q, want empty", res.Thinking)
	}
	if res.Outcome != roundOutcomeText {
		t.Fatalf("outcome = %v, want roundOutcomeText", res.Outcome)
	}
}

// TestRunAIRound_FlushesPendingOnCrossRoundSkip reproduces Task 5 issue 1:
// round 1 is a media round (write_doc) whose canned explanation is deferred
// into pending; the continuation round returns a web_search tool call, round 2
// is text-only and shouldSkipToolLLM returns true — the accumulated pending
// canned explanation must still be flushed before the early return.
func TestRunAIRound_FlushesPendingOnCrossRoundSkip(t *testing.T) {
	db := testSinkStore(t)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch calls.Add(1) {
		case 1:
			// First call: media round — write a doc (explanation deferred).
			json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{
					"message": map[string]any{
						"role": "assistant",
						"tool_calls": []map[string]any{{
							"id":       "call_1",
							"type":     "function",
							"function": map[string]any{"name": "write_doc", "arguments": `{"title":"a","content":"# hi"}`},
						}},
					},
					"finish_reason": "tool_calls",
				}},
			})
		case 2:
			// Second call: the continuation returns a text-only web_search call.
			json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{
					"message": map[string]any{
						"role": "assistant",
						"tool_calls": []map[string]any{{
							"id":       "call_2",
							"type":     "function",
							"function": map[string]any{"name": "web_search", "arguments": `{"query":"天气"}`},
						}},
					},
					"finish_reason": "tool_calls",
				}},
			})
		default:
			// A third call means the skip logic regressed.
			w.WriteHeader(http.StatusInternalServerError)
		}
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

	eng := persona.New(db, nil, fakeCompleter)
	eng.SetSearcher(fakeProactiveSearcher{
		results: []persona.SearchResult{{Title: "x", Snippet: "今天有雨"}},
	})
	s := &AI{Store: db, Persona: eng}
	p := &recordingProvider{}
	d := Delivery{
		BotDBID:  "bot1",
		Provider: p,
		Message:  provider.InboundMessage{Sender: "wxid_a", ContextToken: "ctx-1"},
	}
	// collectTools() early-returns nil when AppDisp is unset, so the persona
	// tools are taken straight from the engine (the same set collectTools
	// would append to the app tools).
	tools := eng.Tools()
	cfg := store.AIConfig{APIKey: "test-key", BaseURL: srv.URL, Model: "test-model"}
	msgs := []aiinternal.Message{{Role: "user", Content: "帮我整理下"}}
	res, err := s.runAIRound(context.Background(), d, cfg, msgs, tools, nil, nil, nil)
	if err != nil {
		t.Fatalf("runAIRound: %v", err)
	}
	if res.Outcome != roundOutcomeDelivered {
		t.Fatalf("outcome = %v, want roundOutcomeDelivered", res.Outcome)
	}

	got := p.messages()
	hasFile := false
	hasSearchText := false
	hasCanned := false
	for _, m := range got {
		if m.Data != nil && m.FileName == "a.md" {
			hasFile = true
		}
		if strings.Contains(m.Text, "今天有雨") {
			hasSearchText = true
		}
		if strings.Contains(m.Text, "我整理好了，你直接看这个文档就行。") {
			hasCanned = true
		}
	}
	if !hasFile {
		t.Errorf("expected the write_doc file (a.md), got %+v", got)
	}
	if !hasSearchText {
		t.Errorf("expected the web_search conclusion text, got %+v", got)
	}
	if !hasCanned {
		t.Errorf("expected the flushed canned write_doc explanation (pending must survive the cross-round skip), got %+v", got)
	}
	if gotCalls := calls.Load(); gotCalls != 2 {
		t.Errorf("LLM api calls = %d, want exactly 2", gotCalls)
	}
}

// TestReply_NoErrorNoticeWhenFallbackDelivered reproduces Task 5 issue 2: the
// LLM returns a gen_image tool call (media round), the continuation round then
// fails with HTTP 500 — the canned explanation is flushed as the fallback, so
// reply() must NOT additionally send the generic error notice.
func TestReply_NoErrorNoticeWhenFallbackDelivered(t *testing.T) {
	db := testSinkStore(t)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			// First call: media round — generate an image (explanation deferred).
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
		// Second call: the continuation round fails.
		w.WriteHeader(http.StatusInternalServerError)
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

	eng := persona.New(db, nil, fakeCompleter)
	eng.SetImageProvider(&fakeProactiveImage{
		images: [][]byte{{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 1}},
	})
	s := &AI{Store: db, Persona: eng}
	p := &recordingProvider{}
	d := Delivery{
		BotDBID:   "bot1",
		Provider:  p,
		Message:   provider.InboundMessage{Sender: "wxid_a"},
		SeqID:     1,
		MsgType:   "text",
		Content:   "画个图",
		AIEnabled: true,
	}
	s.reply(d)

	got := p.messages()
	hasImage := false
	hasCanned := false
	for _, m := range got {
		if m.Data != nil && m.FileName == "image.png" {
			hasImage = true
		}
		if strings.Contains(m.Text, "我先把图发你，你看看是不是这个感觉。") {
			hasCanned = true
		}
		if strings.Contains(m.Text, "回复失败") {
			t.Errorf("error notice must not be sent when the fallback was delivered: %+v", got)
		}
	}
	if !hasImage {
		t.Errorf("expected the gen_image image message, got %+v", got)
	}
	if !hasCanned {
		t.Errorf("expected the flushed canned gen_image explanation, got %+v", got)
	}
	if gotCalls := calls.Load(); gotCalls != 2 {
		t.Errorf("LLM api calls = %d, want exactly 2", gotCalls)
	}
}

// TestReply_ErrorNoticeWhenFallbackSendFails: media round where the canned
// fallback explanation send FAILS (text sends fail, the image send still
// succeeds). The user received the image but no explanation, so reply() must
// still send the generic error notice — roundOutcomeDelivered is only valid
// when the fallback flush actually reached the user.
func TestReply_ErrorNoticeWhenFallbackSendFails(t *testing.T) {
	db := testSinkStore(t)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			// First call: media round — generate an image (explanation deferred).
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
		// Second call: the continuation round fails.
		w.WriteHeader(http.StatusInternalServerError)
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

	eng := persona.New(db, nil, fakeCompleter)
	eng.SetImageProvider(&fakeProactiveImage{
		images: [][]byte{{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 1}},
	})
	s := &AI{Store: db, Persona: eng}
	// Only the canned fallback explanation fails to send; the image (media)
	// send and the error notice (text) still succeed.
	p := &recordingProvider{failTextContains: "我先把图发你"}
	d := Delivery{
		BotDBID:   "bot1",
		Provider:  p,
		Message:   provider.InboundMessage{Sender: "wxid_a"},
		SeqID:     1,
		MsgType:   "text",
		Content:   "画个图",
		AIEnabled: true,
	}
	s.reply(d)

	got := p.messages()
	found := false
	for _, m := range got {
		if strings.Contains(m.Text, "回复失败") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the error notice when the fallback flush failed, got %+v", got)
	}
	if gotCalls := calls.Load(); gotCalls != 2 {
		t.Errorf("LLM api calls = %d, want exactly 2", gotCalls)
	}
}

// TestReply_ErrorNoticeWhenNothingDelivered: a silent persona round (remember)
// where the continuation round fails. Nothing user-facing was delivered and
// pending is empty, so reply() must send the generic error notice instead of
// reporting roundOutcomeDelivered.
func TestReply_ErrorNoticeWhenNothingDelivered(t *testing.T) {
	db := testSinkStore(t)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			// First call: silent persona round — remember a fact (nothing
			// user-facing is delivered).
			json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{
					"message": map[string]any{
						"role": "assistant",
						"tool_calls": []map[string]any{{
							"id":       "call_1",
							"type":     "function",
							"function": map[string]any{"name": "remember", "arguments": `{"content":"x"}`},
						}},
					},
					"finish_reason": "tool_calls",
				}},
			})
			return
		}
		// Second call: the continuation round fails.
		w.WriteHeader(http.StatusInternalServerError)
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

	eng := persona.New(db, nil, fakeCompleter)
	s := &AI{Store: db, Persona: eng}
	// Create the bot so the silent remember tool's memory insert satisfies its
	// FK on bots(id).
	bot, err := db.CreateBot("user1", "bot1", "ilink", "wxid_bot1", nil)
	if err != nil {
		t.Fatal(err)
	}
	p := &recordingProvider{}
	d := Delivery{
		BotDBID:   bot.ID,
		Provider:  p,
		Message:   provider.InboundMessage{Sender: "wxid_a"},
		SeqID:     1,
		MsgType:   "text",
		Content:   "记得这个",
		AIEnabled: true,
	}
	s.reply(d)

	got := p.messages()
	found := false
	for _, m := range got {
		if strings.Contains(m.Text, "回复失败") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the error notice when nothing was delivered, got %+v", got)
	}
	if gotCalls := calls.Load(); gotCalls != 2 {
		t.Errorf("LLM api calls = %d, want exactly 2", gotCalls)
	}
}
