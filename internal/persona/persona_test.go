package persona

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openilink/openilink-hub/internal/ai"
	"github.com/openilink/openilink-hub/internal/store"
	"github.com/openilink/openilink-hub/internal/store/sqlite"
)

type fakeSearcher struct {
	results []SearchResult
	err     error
}

func (f fakeSearcher) Search(ctx context.Context, query string) ([]SearchResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.results, nil
}

type fakeImageProvider struct {
	images [][]byte
	err    error
	calls  int
}

func (f *fakeImageProvider) Generate(ctx context.Context, prompt, negativePrompt string, opts ImageOptions) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.calls >= len(f.images) {
		return nil, errors.New("no fake image prepared")
	}
	img := append([]byte(nil), f.images[f.calls]...)
	f.calls++
	return img, nil
}

func fakePNG(seed byte) []byte {
	return []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', seed}
}

type recordStorage struct {
	keys         []string
	contentTypes []string
	payloads     [][]byte
}

func (s *recordStorage) Put(_ context.Context, key, contentType string, data []byte) (string, error) {
	s.keys = append(s.keys, key)
	s.contentTypes = append(s.contentTypes, contentType)
	s.payloads = append(s.payloads, append([]byte(nil), data...))
	return "memory://" + key, nil
}

func (s *recordStorage) Get(_ context.Context, key string) ([]byte, error) {
	for i, k := range s.keys {
		if k == key {
			return append([]byte(nil), s.payloads[i]...), nil
		}
	}
	return nil, errors.New("not found")
}

func (s *recordStorage) URL(key string) string { return "memory://" + key }

func newTestEngine(t *testing.T, threshold int) (*Engine, *sqlite.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "persona.db")
	db, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	// Persona tables reference bots(id) with ON DELETE CASCADE — create a real
	// user + bot so FK constraints are satisfied.
	u, err := db.CreateUser("persona_test_owner", "Persona Test")
	if err != nil {
		t.Fatal(err)
	}
	b, err := db.CreateBot(u.ID, "PersonaBot", "test", "wx_bot1", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}

	for k, v := range map[string]string{
		"ai.api_key":                      "test-key",
		"ai.base_url":                     "https://example.invalid/v1",
		"ai.memory_consolidate_threshold": itoa(threshold),
	} {
		if err := db.SetConfig(k, v); err != nil {
			t.Fatal(err)
		}
	}

	e := New(db, nil, func(ctx context.Context, cfg store.AIConfig, msgs []ai.Message, tools []ai.Tool) (*ai.CompletionResult, error) {
		return &ai.CompletionResult{Content: "[]"}, nil
	})
	return e, db, b.ID
}

func TestRemember_AddsMemory(t *testing.T) {
	e, db, botID := newTestEngine(t, 100)
	res := e.Execute(context.Background(), botID, "wxid_a", "remember",
		json.RawMessage(`{"category":"profile","content":"怕坐飞机","importance":3,"expires_at":"2026-09-01"}`))
	if !strings.Contains(res.Content, "ok") {
		t.Fatalf("remember result = %q", res.Content)
	}
	mems, _ := db.ListMemories(botID, "wxid_a")
	if len(mems) != 1 {
		t.Fatalf("memories = %d, want 1", len(mems))
	}
	if mems[0].Content != "怕坐飞机" || mems[0].Category != "profile" || mems[0].Importance != 3 {
		t.Errorf("memory mismatch: %+v", mems[0])
	}
	if mems[0].ExpiresAt == nil {
		t.Error("expires_at should be parsed")
	}
}

func TestLookupContact_FiltersExpiredMemories(t *testing.T) {
	// H1: expires_at auto-forget — lookup_contact 不得返回过期记忆。
	e, db, botID := newTestEngine(t, 100)
	now := time.Now()
	past := now.Add(-time.Hour).Unix()
	if err := db.AddMemory(&store.Memory{BotID: botID, Sender: "wxid_a", Category: "fact", Content: "已过期的秘密", ExpiresAt: &past}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddMemory(&store.Memory{BotID: botID, Sender: "wxid_a", Category: "fact", Content: "有效的记忆"}); err != nil {
		t.Fatal(err)
	}
	res := e.Execute(context.Background(), botID, "wxid_b", "lookup_contact", json.RawMessage(`{"sender":"wxid_a"}`))
	if strings.Contains(res.Content, "已过期的秘密") {
		t.Errorf("expired memory must not appear in lookup_contact, got %q", res.Content)
	}
	if !strings.Contains(res.Content, "有效的记忆") {
		t.Errorf("non-expired memory should appear in lookup_contact, got %q", res.Content)
	}
}

func TestScheduleMessage_ProactiveDedup(t *testing.T) {
	e, db, botID := newTestEngine(t, 100)
	e.Execute(context.Background(), botID, "wxid_a", "schedule_message",
		json.RawMessage(`{"fire_at":"2026-08-12 06:30","prompt":"叫主人起床","origin":"proactive"}`))
	e.Execute(context.Background(), botID, "wxid_a", "schedule_message",
		json.RawMessage(`{"fire_at":"2026-08-12 06:45","prompt":"再叫一次","origin":"proactive"}`))

	pending, err := db.ListPendingByScope(botID, "wxid_a", "proactive")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending proactive = %d, want 1 (dedup)", len(pending))
	}
	if pending[0].Prompt != "再叫一次" {
		t.Errorf("newer proactive should replace older, got %q", pending[0].Prompt)
	}

	// promise 不参与去重。
	e.Execute(context.Background(), botID, "wxid_a", "schedule_message",
		json.RawMessage(`{"fire_at":"2026-08-12 06:30","prompt":"用户约定的事","origin":"promise"}`))
	all, _ := db.ListScheduledMessages(botID, 10)
	var promises int
	for _, m := range all {
		if m.Origin == "promise" && m.Status == "pending" {
			promises++
		}
	}
	if promises != 1 {
		t.Errorf("promise count = %d, want 1", promises)
	}
}

func TestExecute_SanitizesReservedDiagnosticsLogFields(t *testing.T) {
	e, _, botID := newTestEngine(t, 100)

	const toolName = "test_sanitize_diagnostics"
	oldHandler, hadOldHandler := toolHandlers[toolName]
	toolHandlers[toolName] = func(ctx context.Context, e *Engine, botID, sender string, args map[string]any) ai.ToolCallResult {
		return ai.ToolCallResult{
			Content: "ok",
			Diagnostics: ai.ToolCallDiagnostics{
				ErrorStage:   "search",
				ErrorMessage: "timeout",
				LogFields: map[string]string{
					"delivery_kind": "text",
					"error_stage":   "duplicated",
					"error_message": "duplicated",
				},
			},
		}
	}
	t.Cleanup(func() {
		if hadOldHandler {
			toolHandlers[toolName] = oldHandler
			return
		}
		delete(toolHandlers, toolName)
	})

	res := e.Execute(context.Background(), botID, "wxid_a", toolName, json.RawMessage(`{}`))
	if res.Diagnostics.LogFields["delivery_kind"] != "text" {
		t.Fatal("expected custom log field to be preserved")
	}
	for _, key := range []string{"error_stage", "error_message"} {
		if _, ok := res.Diagnostics.LogFields[key]; ok {
			t.Fatalf("reserved diagnostics key %q must be removed from LogFields", key)
		}
	}
}

func TestWebSearch_ReturnsConclusionStyleText(t *testing.T) {
	e, _, botID := newTestEngine(t, 100)
	e.SetSearcher(fakeSearcher{
		results: []SearchResult{
			{
				Title:   "杭州天气",
				URL:     "https://example.com/weather",
				Snippet: "今天杭州有阵雨，最高 28 度。",
			},
			{
				Title:   "杭州气温",
				URL:     "https://example.com/temp",
				Snippet: "空气偏闷，出门最好带伞。",
			},
		},
	})

	res := e.Execute(context.Background(), botID, "wxid_a", "web_search", json.RawMessage(`{"query":"杭州天气"}`))
	if strings.Contains(res.Content, "https://") {
		t.Fatal("expected no source url in conclusion-style reply")
	}
	if strings.Contains(res.Delivery.Explanation, "https://") {
		t.Fatal("expected no source url in concise delivery explanation")
	}
	if res.Delivery.Explanation == "" {
		t.Fatal("expected concise delivery explanation")
	}
	if res.Delivery.Explanation == res.Content {
		t.Fatal("expected delivery explanation to stay shorter than continuation content")
	}
	if len([]rune(res.Delivery.Explanation)) >= len([]rune(res.Content)) {
		t.Fatalf("expected continuation content to be richer than explanation, got explanation=%q content=%q", res.Delivery.Explanation, res.Content)
	}
	if !res.Delivery.UserFacing {
		t.Fatal("expected search result to be marked user-facing")
	}
	if res.Diagnostics.LogFields["delivery_kind"] != "text" {
		t.Fatal("expected text delivery metadata")
	}
}

func TestWebSearchTool_DescriptionUsesConclusionStyleGuidance(t *testing.T) {
	desc := webSearchTool.Function.Description
	if strings.Contains(desc, "返回摘要") {
		t.Fatalf("web_search description should avoid summary-style wording, got %q", desc)
	}
	for _, want := range []string{"只给结论", "不列来源"} {
		if !strings.Contains(desc, want) {
			t.Fatalf("web_search description should mention %q, got %q", want, desc)
		}
	}
}

func TestWebSearch_StripsEmbeddedURLsFromConclusionText(t *testing.T) {
	e, _, botID := newTestEngine(t, 100)
	e.SetSearcher(fakeSearcher{
		results: []SearchResult{{
			Title:   "杭州天气",
			URL:     "https://example.com/weather",
			Snippet: "今天杭州有阵雨。https://example.com/weather 最高 28 度。",
		}},
	})

	res := e.Execute(context.Background(), botID, "wxid_a", "web_search", json.RawMessage(`{"query":"杭州天气"}`))
	if strings.Contains(res.Content, "https://") {
		t.Fatalf("expected embedded urls to be stripped from search conclusion, got %q", res.Content)
	}
	if !strings.Contains(res.Content, "今天杭州有阵雨") || !strings.Contains(res.Content, "最高 28 度") {
		t.Fatalf("expected conclusion to preserve useful content, got %q", res.Content)
	}
}

func TestNormalizeSearchConclusionPart_PreservesChineseTextAdjacentToURLPunctuation(t *testing.T) {
	got := normalizeSearchConclusionPart("详见：https://example.com，中文说明继续。")
	if got != "详见：中文说明继续" {
		t.Fatalf("normalizeSearchConclusionPart() = %q, want %q", got, "详见：中文说明继续")
	}
}

func TestWebSearch_DropsTitlePrefixCardFormattingFromSummary(t *testing.T) {
	e, _, botID := newTestEngine(t, 100)
	e.SetSearcher(fakeSearcher{
		results: []SearchResult{{
			Title:   "杭州天气",
			URL:     "https://example.com/weather",
			Snippet: "杭州天气 - 今天有阵雨，最高 28 度。",
		}},
	})

	res := e.Execute(context.Background(), botID, "wxid_a", "web_search", json.RawMessage(`{"query":"杭州天气"}`))
	if strings.Contains(res.Content, "杭州天气 - ") || strings.Contains(res.Delivery.Explanation, "杭州天气 - ") {
		t.Fatalf("expected title-prefix card formatting to be removed, got content=%q explanation=%q", res.Content, res.Delivery.Explanation)
	}
	if !strings.Contains(res.Content, "今天有阵雨，最高 28 度") {
		t.Fatalf("expected summary to keep description text, got %q", res.Content)
	}
}

func TestWebSearch_ContentKeepsRicherEvidenceWithoutURLs(t *testing.T) {
	e, _, botID := newTestEngine(t, 100)
	e.SetSearcher(fakeSearcher{
		results: []SearchResult{
			{
				Title:   "杭州天气",
				URL:     "https://example.com/weather",
				Snippet: "今天杭州有阵雨，最高 28 度。",
			},
			{
				Title:   "杭州出行提醒",
				URL:     "https://example.com/tips",
				Snippet: "晚一点雨会更明显，出门带伞更稳妥。",
			},
		},
	})

	res := e.Execute(context.Background(), botID, "wxid_a", "web_search", json.RawMessage(`{"query":"杭州天气"}`))
	if !strings.Contains(res.Content, "今天杭州有阵雨") {
		t.Fatalf("expected continuation content to keep primary evidence, got %q", res.Content)
	}
	if !strings.Contains(res.Content, "晚一点雨会更明显") {
		t.Fatalf("expected continuation content to keep richer follow-up evidence, got %q", res.Content)
	}
	if strings.Contains(res.Content, "https://") || strings.Contains(res.Delivery.Explanation, "https://") {
		t.Fatalf("expected search output to strip urls, got content=%q explanation=%q", res.Content, res.Delivery.Explanation)
	}
	if strings.Contains(res.Delivery.Explanation, "晚一点雨会更明显") {
		t.Fatalf("expected concise explanation to avoid full evidence dump, got %q", res.Delivery.Explanation)
	}
}

func TestWebSearch_ReturnsNaturalFailureAndDiagnostics(t *testing.T) {
	e, _, botID := newTestEngine(t, 100)
	e.SetSearcher(fakeSearcher{err: errors.New("upstream timeout")})

	res := e.Execute(context.Background(), botID, "wxid_a", "web_search", json.RawMessage(`{"query":"杭州天气"}`))
	if strings.Contains(res.Content, "timeout") || strings.Contains(res.Content, "web_search") {
		t.Fatalf("expected natural failure text, got %q", res.Content)
	}
	if !res.Delivery.UserFacing {
		t.Fatal("expected failed search to remain user-facing")
	}
	if res.Diagnostics.ErrorStage != "search" {
		t.Fatalf("error stage = %q, want search", res.Diagnostics.ErrorStage)
	}
	if res.Diagnostics.ErrorMessage != "upstream timeout" {
		t.Fatalf("error message = %q, want upstream timeout", res.Diagnostics.ErrorMessage)
	}
	if res.Diagnostics.LogFields["delivery_kind"] != "text" {
		t.Fatal("expected text delivery metadata")
	}
}

func TestWriteDoc_ReturnsMarkdownFileAndExplanation(t *testing.T) {
	e, _, botID := newTestEngine(t, 100)

	res := e.Execute(context.Background(), botID, "wxid_a", "write_doc", json.RawMessage(`{
		"title":"旅行计划",
		"content":"# 周末旅行\n\n- 带雨伞"
	}`))

	if len(res.Files) != 1 {
		t.Fatalf("files = %d, want 1", len(res.Files))
	}
	if res.Files[0].FileName != "旅行计划.md" {
		t.Fatalf("file name = %q, want %q", res.Files[0].FileName, "旅行计划.md")
	}
	if res.Files[0].ContentType != "text/markdown" {
		t.Fatalf("content type = %q, want text/markdown", res.Files[0].ContentType)
	}
	if string(res.Files[0].Data) != "# 周末旅行\n\n- 带雨伞" {
		t.Fatalf("file data = %q", string(res.Files[0].Data))
	}
	if res.Delivery.Explanation == "" {
		t.Fatal("expected a natural follow-up sentence")
	}
	if !res.Delivery.UserFacing {
		t.Fatal("expected write_doc result to be user-facing")
	}
	if res.Diagnostics.LogFields["delivery_kind"] != "file" {
		t.Fatal("expected file delivery metadata")
	}
	if res.Diagnostics.LogFields["result_count"] != "1" {
		t.Fatalf("result_count = %q, want 1", res.Diagnostics.LogFields["result_count"])
	}
}

func TestWriteDoc_NormalizesNonMarkdownExtensionToMD(t *testing.T) {
	e, _, botID := newTestEngine(t, 100)

	res := e.Execute(context.Background(), botID, "wxid_a", "write_doc", json.RawMessage(`{
		"title":"旅行计划.txt",
		"content":"# 周末旅行\n\n- 带雨伞"
	}`))

	if len(res.Files) != 1 {
		t.Fatalf("files = %d, want 1", len(res.Files))
	}
	if res.Files[0].FileName != "旅行计划.md" {
		t.Fatalf("file name = %q, want %q", res.Files[0].FileName, "旅行计划.md")
	}
	if res.Files[0].ContentType != "text/markdown" {
		t.Fatalf("content type = %q, want text/markdown", res.Files[0].ContentType)
	}
}

func TestWriteDoc_StripsPathSegmentsForFileAndStorageKey(t *testing.T) {
	e, _, botID := newTestEngine(t, 100)
	st := &recordStorage{}
	e.Storage = st

	res := e.Execute(context.Background(), botID, "wxid_a", "write_doc", json.RawMessage(`{
		"title":"..\\草稿/旅行计划.txt",
		"content":"# 周末旅行\n\n- 带雨伞"
	}`))

	if len(res.Files) != 1 {
		t.Fatalf("files = %d, want 1", len(res.Files))
	}
	if res.Files[0].FileName != "旅行计划.md" {
		t.Fatalf("file name = %q, want %q", res.Files[0].FileName, "旅行计划.md")
	}
	if len(st.keys) != 1 {
		t.Fatalf("stored keys = %d, want 1", len(st.keys))
	}
	if st.keys[0] != "docs/"+botID+"/旅行计划.md" {
		t.Fatalf("storage key = %q, want %q", st.keys[0], "docs/"+botID+"/旅行计划.md")
	}
}

func TestWriteDoc_NormalizesUnsafeTitleCharacters(t *testing.T) {
	e, _, botID := newTestEngine(t, 100)
	st := &recordStorage{}
	e.Storage = st

	res := e.Execute(context.Background(), botID, "wxid_a", "write_doc", json.RawMessage(`{
		"title":"季度总结:Q3?.txt",
		"content":"# 周报"
	}`))

	if len(res.Files) != 1 {
		t.Fatalf("files = %d, want 1", len(res.Files))
	}
	if res.Files[0].FileName != "季度总结-Q3.md" {
		t.Fatalf("file name = %q, want %q", res.Files[0].FileName, "季度总结-Q3.md")
	}
	if len(st.keys) != 1 {
		t.Fatalf("stored keys = %d, want 1", len(st.keys))
	}
	if st.keys[0] != "docs/"+botID+"/季度总结-Q3.md" {
		t.Fatalf("storage key = %q, want %q", st.keys[0], "docs/"+botID+"/季度总结-Q3.md")
	}
}

func TestWriteDoc_FallsBackToSafeMarkdownNameWhenTitleDegenerates(t *testing.T) {
	e, _, botID := newTestEngine(t, 100)
	st := &recordStorage{}
	e.Storage = st

	res := e.Execute(context.Background(), botID, "wxid_a", "write_doc", json.RawMessage(`{
		"title":"../...",
		"content":"# 周报"
	}`))

	if len(res.Files) != 1 {
		t.Fatalf("files = %d, want 1", len(res.Files))
	}
	if res.Files[0].FileName != "document.md" {
		t.Fatalf("file name = %q, want %q", res.Files[0].FileName, "document.md")
	}
	if len(st.keys) != 1 {
		t.Fatalf("stored keys = %d, want 1", len(st.keys))
	}
	if st.keys[0] != "docs/"+botID+"/document.md" {
		t.Fatalf("storage key = %q, want %q", st.keys[0], "docs/"+botID+"/document.md")
	}
}

func TestWriteDoc_ReturnsNaturalErrorWithDiagnostics(t *testing.T) {
	e, _, botID := newTestEngine(t, 100)

	res := e.Execute(context.Background(), botID, "wxid_a", "write_doc", json.RawMessage(`{
		"title":"  ",
		"content":""
	}`))

	if strings.Contains(res.Content, "write_doc") || strings.Contains(res.Content, "required") {
		t.Fatalf("expected natural error text, got %q", res.Content)
	}
	if !res.Delivery.UserFacing {
		t.Fatal("expected invalid write_doc result to remain user-facing")
	}
	if res.Diagnostics.ErrorStage != "validate" {
		t.Fatalf("error stage = %q, want validate", res.Diagnostics.ErrorStage)
	}
	if res.Diagnostics.ErrorMessage == "" {
		t.Fatal("expected diagnostic error message")
	}
	if res.Diagnostics.LogFields["delivery_kind"] != "file" {
		t.Fatal("expected file delivery metadata")
	}
	if res.Diagnostics.LogFields["result_count"] != "0" {
		t.Fatalf("result_count = %q, want 0", res.Diagnostics.LogFields["result_count"])
	}
	if len(res.Files) != 0 {
		t.Fatalf("files = %d, want 0", len(res.Files))
	}
}

func TestGenImage_RespectsRequestedCountAndSetsExplanation(t *testing.T) {
	e, _, botID := newTestEngine(t, 100)
	provider := &fakeImageProvider{
		images: [][]byte{fakePNG(1), fakePNG(2)},
	}
	e.SetImageProvider(provider)

	res := e.Execute(context.Background(), botID, "wxid_a", "gen_image", json.RawMessage(`{
		"prompt":"雨夜街道",
		"count":2
	}`))

	if len(res.Images) != 2 {
		t.Fatalf("images = %d, want 2", len(res.Images))
	}
	if provider.calls != 2 {
		t.Fatalf("provider calls = %d, want 2", provider.calls)
	}
	for i, img := range res.Images {
		if img.ContentType != "image/png" {
			t.Fatalf("image %d content type = %q, want image/png", i, img.ContentType)
		}
	}
	if !res.Delivery.UserFacing {
		t.Fatal("expected gen_image result to be user-facing")
	}
	if res.Delivery.Explanation == "" {
		t.Fatal("expected a natural follow-up sentence")
	}
	if res.Diagnostics.LogFields["delivery_kind"] != "image" {
		t.Fatal("expected image delivery metadata")
	}
	if res.Diagnostics.LogFields["result_count"] != "2" {
		t.Fatalf("result_count = %q, want 2", res.Diagnostics.LogFields["result_count"])
	}
}

func TestGenImage_CapsRequestedCountToSafeMaximum(t *testing.T) {
	e, _, botID := newTestEngine(t, 100)
	images := make([][]byte, maxGeneratedImages+2)
	for i := range images {
		images[i] = fakePNG(byte(i + 1))
	}
	provider := &fakeImageProvider{images: images}
	e.SetImageProvider(provider)

	res := e.Execute(context.Background(), botID, "wxid_a", "gen_image", json.RawMessage(`{
		"prompt":"雨夜街道",
		"count":9
	}`))

	if len(res.Images) != maxGeneratedImages {
		t.Fatalf("images = %d, want %d", len(res.Images), maxGeneratedImages)
	}
	if provider.calls != maxGeneratedImages {
		t.Fatalf("provider calls = %d, want %d", provider.calls, maxGeneratedImages)
	}
	if res.Diagnostics.LogFields["result_count"] != itoa(maxGeneratedImages) {
		t.Fatalf("result_count = %q, want %d", res.Diagnostics.LogFields["result_count"], maxGeneratedImages)
	}
}

func TestGenImageTool_SchemaExposesCountParameter(t *testing.T) {
	var params struct {
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(genImageTool.Function.Parameters, &params); err != nil {
		t.Fatalf("parse gen_image parameters: %v", err)
	}
	count, ok := params.Properties["count"]
	if !ok {
		t.Fatal("gen_image schema must expose a \"count\" property so the model can request multiple images")
	}
	if count.Type != "integer" {
		t.Fatalf("count type = %q, want integer", count.Type)
	}
}

func TestGenImage_ReturnsNaturalFailureAndDiagnostics(t *testing.T) {
	e, _, botID := newTestEngine(t, 100)
	e.SetImageProvider(&fakeImageProvider{err: errors.New("upstream 500")})

	res := e.Execute(context.Background(), botID, "wxid_a", "gen_image", json.RawMessage(`{
		"prompt":"雨夜街道",
		"count":2
	}`))

	if strings.Contains(res.Content, "500") || strings.Contains(res.Content, "gen_image") {
		t.Fatalf("expected natural failure text, got %q", res.Content)
	}
	if !res.Delivery.UserFacing {
		t.Fatal("expected failed gen_image result to remain user-facing")
	}
	if res.Diagnostics.ErrorStage != "generate" {
		t.Fatalf("error stage = %q, want generate", res.Diagnostics.ErrorStage)
	}
	if res.Diagnostics.ErrorMessage != "upstream 500" {
		t.Fatalf("error message = %q, want upstream 500", res.Diagnostics.ErrorMessage)
	}
	if res.Diagnostics.LogFields["delivery_kind"] != "image" {
		t.Fatal("expected image delivery metadata")
	}
}

func TestGenImage_R18Gate_BlocksNonOwner(t *testing.T) {
	e, db, botID := newTestEngine(t, 100)
	if err := db.UpsertContact(&store.Contact{BotID: botID, Sender: "wxid_owner", Name: "主人"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetOwner(botID, "wxid_owner"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertContact(&store.Contact{BotID: botID, Sender: "wxid_friend", Name: "朋友"}); err != nil {
		t.Fatal(err)
	}
	provider := &fakeImageProvider{images: [][]byte{fakePNG(1)}}
	e.SetImageProvider(provider)

	res := e.Execute(context.Background(), botID, "wxid_friend", "gen_image", json.RawMessage(`{
		"prompt":"nude portrait of someone",
		"count":1
	}`))

	if !res.Delivery.UserFacing {
		t.Fatal("expected declined result to remain user-facing")
	}
	if res.Diagnostics.ErrorStage != "policy" {
		t.Fatalf("error stage = %q, want policy", res.Diagnostics.ErrorStage)
	}
	if res.Diagnostics.ErrorMessage != "r18 prompt blocked for non-owner" {
		t.Fatalf("error message = %q, want r18 prompt blocked for non-owner", res.Diagnostics.ErrorMessage)
	}
	if len(res.Images) != 0 {
		t.Fatalf("images = %d, want 0", len(res.Images))
	}
	if provider.calls != 0 {
		t.Fatalf("provider calls = %d, want 0 (blocked before generation)", provider.calls)
	}
	if res.Diagnostics.LogFields["delivery_kind"] != "image" {
		t.Fatal("expected image delivery metadata")
	}
	if res.Diagnostics.LogFields["result_count"] != "0" {
		t.Fatalf("result_count = %q, want 0", res.Diagnostics.LogFields["result_count"])
	}
	if res.Delivery.Explanation == "" {
		t.Fatal("expected a natural decline sentence")
	}
}

func TestGenImage_R18Gate_BlocksNonOwnerChineseKeyword(t *testing.T) {
	e, db, botID := newTestEngine(t, 100)
	if err := db.UpsertContact(&store.Contact{BotID: botID, Sender: "wxid_owner"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetOwner(botID, "wxid_owner"); err != nil {
		t.Fatal(err)
	}
	provider := &fakeImageProvider{images: [][]byte{fakePNG(1)}}
	e.SetImageProvider(provider)

	res := e.Execute(context.Background(), botID, "wxid_other", "gen_image", json.RawMessage(`{
		"prompt":"好看的风景",
		"negative_prompt":"色情",
		"count":1
	}`))

	if res.Diagnostics.ErrorStage != "policy" {
		t.Fatalf("error stage = %q, want policy (negative_prompt keyword must be scanned)", res.Diagnostics.ErrorStage)
	}
	if len(res.Images) != 0 {
		t.Fatalf("images = %d, want 0", len(res.Images))
	}
}

func TestGenImage_R18Gate_NonOwnerNormalPromptGenerates(t *testing.T) {
	e, db, botID := newTestEngine(t, 100)
	if err := db.UpsertContact(&store.Contact{BotID: botID, Sender: "wxid_owner", Name: "主人"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetOwner(botID, "wxid_owner"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertContact(&store.Contact{BotID: botID, Sender: "wxid_friend"}); err != nil {
		t.Fatal(err)
	}
	provider := &fakeImageProvider{images: [][]byte{fakePNG(1)}}
	e.SetImageProvider(provider)

	res := e.Execute(context.Background(), botID, "wxid_friend", "gen_image", json.RawMessage(`{
		"prompt":"雨夜街道",
		"count":1
	}`))

	if len(res.Images) != 1 {
		t.Fatalf("images = %d, want 1 (normal prompt must pass)", len(res.Images))
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", provider.calls)
	}
}

func TestGenImage_R18Gate_OwnerPasses(t *testing.T) {
	e, db, botID := newTestEngine(t, 100)
	if err := db.UpsertContact(&store.Contact{BotID: botID, Sender: "wxid_owner", Name: "主人"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetOwner(botID, "wxid_owner"); err != nil {
		t.Fatal(err)
	}
	provider := &fakeImageProvider{images: [][]byte{fakePNG(1)}}
	e.SetImageProvider(provider)

	res := e.Execute(context.Background(), botID, "wxid_owner", "gen_image", json.RawMessage(`{
		"prompt":"nude portrait",
		"count":1
	}`))

	if len(res.Images) != 1 {
		t.Fatalf("images = %d, want 1 (owner must pass the r18 gate)", len(res.Images))
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", provider.calls)
	}
}

func TestCancelSchedule_OnlyCancelsOwnPendingMessage(t *testing.T) {
	e, db, botID := newTestEngine(t, 100)
	res := e.Execute(context.Background(), botID, "wxid_owner", "schedule_message",
		json.RawMessage(`{"fire_at":"2026-08-12 08:00","prompt":"自己的提醒","origin":"promise"}`))
	if !strings.Contains(res.Content, "id=") {
		t.Fatalf("schedule result = %q", res.Content)
	}
	start := strings.Index(res.Content, "id=")
	end := strings.Index(res.Content[start:], "，")
	id := res.Content[start+3 : start+end]

	other := &store.ScheduledMessage{
		BotID:  botID,
		Sender: "wxid_other",
		FireAt: time.Now().Add(time.Hour).Unix(),
		Prompt: "别人的提醒",
		Origin: "promise",
		Status: "pending",
	}
	if err := db.AddScheduledMessage(other); err != nil {
		t.Fatalf("AddScheduledMessage(other): %v", err)
	}

	res = e.Execute(context.Background(), botID, "wxid_owner", "cancel_schedule", json.RawMessage(`{"id":"`+other.ID+`"}`))
	if !strings.Contains(res.Content, "not found") {
		t.Fatalf("cross-sender cancel should fail, got %q", res.Content)
	}

	pendingOther, _ := db.ListPendingByScope(botID, "wxid_other", "promise")
	if len(pendingOther) != 1 {
		t.Fatalf("other sender pending count = %d, want 1", len(pendingOther))
	}

	res = e.Execute(context.Background(), botID, "wxid_owner", "cancel_schedule", json.RawMessage(`{"id":"`+id+`"}`))
	if !strings.Contains(res.Content, "已取消") {
		t.Fatalf("own cancel result = %q", res.Content)
	}
	pendingOwn, _ := db.ListPendingByScope(botID, "wxid_owner", "promise")
	if len(pendingOwn) != 0 {
		t.Fatalf("own pending count = %d, want 0", len(pendingOwn))
	}
}

func TestConsolidation_RecentProtected(t *testing.T) {
	e, db, botID := newTestEngine(t, 5)

	// Seed: 15 recent + 3 stale. The recent window is the newest 15 OR anything
	// within 7 days (design §7.2).
	now := time.Now()
	var recentIDs, staleIDs []string
	for i := 0; i < 15; i++ {
		m := store.Memory{BotID: botID, Sender: "wxid_a", Category: "fact", Content: "近期" + itoa(i), Importance: 2}
		db.AddMemory(&m)
		recentIDs = append(recentIDs, m.ID)
	}
	for i := 0; i < 3; i++ {
		m := store.Memory{BotID: botID, Sender: "wxid_a", Category: "detail", Content: "陈旧" + itoa(i), Importance: 1, CreatedAt: now.Add(-30 * 24 * time.Hour).Unix()}
		db.AddMemory(&m)
		staleIDs = append(staleIDs, m.ID)
	}

	// Fake AI: delete the stale ones AND (maliciously) one recent.
	ops := []map[string]any{
		{"action": "DELETE", "ids": staleIDs},
		{"action": "DELETE", "ids": []string{recentIDs[0]}}, // 越界：删近期
	}
	raw, _ := json.Marshal(ops)
	e.Complete = func(ctx context.Context, cfg store.AIConfig, msgs []ai.Message, tools []ai.Tool) (*ai.CompletionResult, error) {
		return &ai.CompletionResult{Content: string(raw)}, nil
	}

	e.consolidate(botID, "wxid_a")

	left, _ := db.ListMemories(botID, "wxid_a")
	var contents []string
	for _, m := range left {
		contents = append(contents, m.Content)
	}
	joined := strings.Join(contents, "|")
	for i := 0; i < 3; i++ {
		if strings.Contains(joined, "陈旧"+itoa(i)) {
			t.Errorf("stale memory 陈旧%d should have been deleted", i)
		}
	}
	for i := 0; i < 15; i++ {
		if !strings.Contains(joined, "近期"+itoa(i)) {
			t.Errorf("recent memory 近期%d must be protected from deletion", i)
		}
	}
}

func TestConsolidation_CompressStale(t *testing.T) {
	e, db, botID := newTestEngine(t, 5)
	now := time.Now()
	var staleIDs []string
	// Fill the recent window first so the old memories below land in the stale
	// region (recent = newest 15 OR within 7 days).
	for i := 0; i < 15; i++ {
		m := store.Memory{BotID: botID, Sender: "wxid_a", Category: "fact", Content: "近期记录", Importance: 2}
		db.AddMemory(&m)
	}
	for i := 0; i < 3; i++ {
		m := store.Memory{BotID: botID, Sender: "wxid_a", Category: "detail", Content: "旧工作忙", Importance: 1, CreatedAt: now.Add(-60 * 24 * time.Hour).Unix()}
		db.AddMemory(&m)
		staleIDs = append(staleIDs, m.ID)
	}

	ops := []map[string]any{
		{"action": "COMPRESS", "ids": staleIDs, "new_content": "长期高压工作状态"},
	}
	raw, _ := json.Marshal(ops)
	e.Complete = func(ctx context.Context, cfg store.AIConfig, msgs []ai.Message, tools []ai.Tool) (*ai.CompletionResult, error) {
		return &ai.CompletionResult{Content: string(raw)}, nil
	}

	e.consolidate(botID, "wxid_a")

	left, _ := db.ListMemories(botID, "wxid_a")
	var contents []string
	for _, m := range left {
		contents = append(contents, m.Content)
	}
	if strings.Contains(strings.Join(contents, "|"), "旧工作忙") {
		t.Error("stale memories should be compressed away")
	}
	found := false
	for _, c := range contents {
		if strings.Contains(c, "长期高压工作状态") {
			found = true
		}
	}
	if !found {
		t.Error("compressed merged memory should exist")
	}
}

func TestExecute_SetsResultName(t *testing.T) {
	e, _, botID := newTestEngine(t, 100)
	e.SetSearcher(fakeSearcher{
		results: []SearchResult{{Title: "杭州天气", URL: "https://example.com/weather", Snippet: "今天杭州有阵雨。"}},
	})

	res := e.Execute(context.Background(), botID, "wxid_a", "web_search", json.RawMessage(`{"query":"杭州天气"}`))
	if res.Name != "web_search" {
		t.Fatalf("res.Name = %q, want %q", res.Name, "web_search")
	}
}

func TestParseTime(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"2026-08-12 06:30", "2026-08-12 06:30"},
		{"2026-08-12", "2026-08-12 00:00"},
		{"2026-08-12T06:30:00+08:00", "2026-08-12 06:30"},
	}
	for _, tc := range cases {
		got, err := parseTime(tc.in)
		if err != nil {
			t.Errorf("parseTime(%q): %v", tc.in, err)
			continue
		}
		if got.Format("2006-01-02 15:04") != tc.want {
			t.Errorf("parseTime(%q) = %s, want %s", tc.in, got.Format("2006-01-02 15:04"), tc.want)
		}
	}
	if _, err := parseTime("not-a-time"); err == nil {
		t.Error("expected error for garbage input")
	}
}

func TestScheduleMessage_SetsDeliveryDiagnostics(t *testing.T) {
	e, _, botID := newTestEngine(t, 100)
	res := e.Execute(context.Background(), botID, "wxid_a", "schedule_message",
		json.RawMessage(`{"fire_at":"2026-08-12 06:30","prompt":"叫起床","origin":"promise"}`))
	if res.Delivery.UserFacing {
		t.Fatal("scheduling should stay silent (not user-facing)")
	}
	if res.Diagnostics.LogFields["delivery_kind"] != "schedule" {
		t.Fatalf("delivery_kind = %q, want schedule", res.Diagnostics.LogFields["delivery_kind"])
	}
	if res.Diagnostics.LogFields["origin"] != "promise" {
		t.Fatalf("origin = %q, want promise", res.Diagnostics.LogFields["origin"])
	}
	if res.Diagnostics.LogFields["result_count"] != "1" {
		t.Fatalf("result_count = %q, want 1", res.Diagnostics.LogFields["result_count"])
	}
}

func TestScheduleMessage_InvalidInputHasDiagnostics(t *testing.T) {
	e, _, botID := newTestEngine(t, 100)
	res := e.Execute(context.Background(), botID, "wxid_a", "schedule_message", json.RawMessage(`{"fire_at":"","prompt":""}`))
	if res.Diagnostics.ErrorStage != "validate" {
		t.Fatalf("error stage = %q, want validate", res.Diagnostics.ErrorStage)
	}
	if res.Diagnostics.LogFields["delivery_kind"] != "schedule" {
		t.Fatalf("delivery_kind = %q, want schedule", res.Diagnostics.LogFields["delivery_kind"])
	}
}

func TestCancelSchedule_SetsDeliveryDiagnostics(t *testing.T) {
	e, _, botID := newTestEngine(t, 100)
	res := e.Execute(context.Background(), botID, "wxid_a", "cancel_schedule", json.RawMessage(`{"id":"missing"}`))
	if res.Diagnostics.LogFields["delivery_kind"] != "schedule" {
		t.Fatalf("delivery_kind = %q, want schedule", res.Diagnostics.LogFields["delivery_kind"])
	}
	if res.Diagnostics.ErrorStage != "cancel" {
		t.Fatalf("error stage = %q, want cancel", res.Diagnostics.ErrorStage)
	}
	if res.Diagnostics.ErrorMessage == "" {
		t.Fatal("expected diagnostic error message for not-found cancel")
	}
}
