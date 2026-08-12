package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/openilink/openilink-hub/internal/character"
	"github.com/openilink/openilink-hub/internal/store"
)

// mockPersonaStore implements the persona-related stores for testing.
type mockPersonaStore struct {
	mockMessageStore
	chars     []store.Character
	world     []store.WorldEntry
	contacts  []store.Contact
	relations []store.ContactRelation
	memories  []store.Memory
	rules     *store.PersonaRules
}

func (m *mockPersonaStore) GetCharacter(id string) (*store.Character, error) {
	for i := range m.chars {
		if m.chars[i].ID == id {
			return &m.chars[i], nil
		}
	}
	return nil, store.ErrNotFound
}
func (m *mockPersonaStore) SaveCharacter(*store.Character) error       { return nil }
func (m *mockPersonaStore) ListCharacters() ([]store.Character, error) { return m.chars, nil }
func (m *mockPersonaStore) DeleteCharacter(string) error               { return nil }
func (m *mockPersonaStore) ListWorldEntries(string) ([]store.WorldEntry, error) {
	return m.world, nil
}
func (m *mockPersonaStore) SaveWorldEntry(*store.WorldEntry) error { return nil }
func (m *mockPersonaStore) DeleteWorldEntry(string) error          { return nil }

func (m *mockPersonaStore) UpsertContact(*store.Contact) error { return nil }
func (m *mockPersonaStore) GetContact(botID, sender string) (*store.Contact, error) {
	for i := range m.contacts {
		if m.contacts[i].BotID == botID && m.contacts[i].Sender == sender {
			return &m.contacts[i], nil
		}
	}
	return nil, store.ErrNotFound
}
func (m *mockPersonaStore) ListContacts(botID string) ([]store.Contact, error) {
	return m.contacts, nil
}
func (m *mockPersonaStore) SetOwner(string, string) error               { return nil }
func (m *mockPersonaStore) DeleteContact(string, string) error          { return nil }
func (m *mockPersonaStore) UpsertRelation(*store.ContactRelation) error { return nil }
func (m *mockPersonaStore) ListRelations(botID string) ([]store.ContactRelation, error) {
	return m.relations, nil
}
func (m *mockPersonaStore) DeleteRelation(string) error { return nil }

func (m *mockPersonaStore) AddMemory(*store.Memory) error { return nil }
func (m *mockPersonaStore) ListMemories(botID, sender string) ([]store.Memory, error) {
	var out []store.Memory
	for _, mem := range m.memories {
		if mem.BotID == botID && mem.Sender == sender {
			out = append(out, mem)
		}
	}
	return out, nil
}
func (m *mockPersonaStore) ListMemoriesByBot(botID string) ([]store.Memory, error) {
	var out []store.Memory
	for _, mem := range m.memories {
		if mem.BotID == botID {
			out = append(out, mem)
		}
	}
	return out, nil
}
func (m *mockPersonaStore) UpdateMemory(*store.Memory) error          { return nil }
func (m *mockPersonaStore) DeleteMemory(string) error                 { return nil }
func (m *mockPersonaStore) CountMemories(string, string) (int, error) { return 0, nil }

func (m *mockPersonaStore) GetPersonaRules(string) (*store.PersonaRules, error) {
	if m.rules != nil {
		return m.rules, nil
	}
	// No rules configured → disabled (true degradation path).
	return &store.PersonaRules{ID: "default", Enabled: false, RulesText: ""}, nil
}
func (m *mockPersonaStore) SavePersonaRules(*store.PersonaRules) error { return nil }

func cardJSON(t *testing.T, name, desc string, book bool) string {
	t.Helper()
	data := map[string]any{
		"name":        name,
		"description": desc,
	}
	envelope := map[string]any{"spec": "chara_card_v2", "spec_version": "2.0", "data": data}
	if book {
		data["character_book"] = map[string]any{
			"entries": []map[string]any{
				{
					"keys":    []string{"日本"},
					"content": "她下周去日本出差",
					"enabled": true,
				},
				{
					"keys":    []string{"小猫"},
					"content": "她养了一只猫叫团子",
					"enabled": true,
				},
			},
		}
	}
	b, _ := json.Marshal(envelope)
	return string(b)
}

func TestBuildMessagesPersona_LayerOrder(t *testing.T) {
	s := &mockPersonaStore{
		chars: []store.Character{{
			ID: "c1", Name: "小爱", CardJSON: cardJSON(t, "小爱", "温柔的女友", false),
		}},
		rules: &store.PersonaRules{ID: "default", Enabled: true, RulesText: "严禁 NTR；只专一"},
		contacts: []store.Contact{
			{BotID: "bot1", Sender: "wxid_zhang", Name: "张三", Brief: "主人", IsOwner: true},
			{BotID: "bot1", Sender: "wxid_li", Name: "李四", Brief: "张三女友"},
		},
		relations: []store.ContactRelation{
			{BotID: "bot1", FromSender: "wxid_zhang", ToSender: "wxid_li", Relation: "情侣"},
		},
		memories: []store.Memory{
			{BotID: "bot1", Sender: "wxid_zhang", Category: "profile", Content: "怕坐飞机", Importance: 3},
		},
	}

	cfg := store.AIConfig{CharacterID: "c1", MemoryScope: "global", HumanizeWechat: true, SystemPrompt: "全局兜底"}
	msgs := BuildMessagesPersona(context.Background(), cfg, s, "bot1", "ch1", "wxid_zhang", "在吗", nil, nil)

	var systemText strings.Builder
	for _, m := range msgs {
		if m.Role == "system" {
			content, _ := m.Content.(string)
			systemText.WriteString(content)
			systemText.WriteString("\n@@@\n")
		}
	}
	joined := systemText.String()

	// 铁律置顶。
	if !strings.HasPrefix(joined, "【铁律】") {
		t.Errorf("rules layer must be first, got prefix %q", joined[:min(20, len(joined))])
	}
	if !strings.Contains(joined, "严禁 NTR") {
		t.Error("rules text missing")
	}
	// 角色卡层。
	if !strings.Contains(joined, "你是小爱") || !strings.Contains(joined, "温柔的女友") {
		t.Error("character layer missing")
	}
	// 微信口语体指令。
	if !strings.Contains(joined, "句尾不加句号") {
		t.Error("wechat style instruction missing")
	}
	// 关系树骨架。
	if !strings.Contains(joined, "张三") || !strings.Contains(joined, "主人") {
		t.Error("relation skeleton missing")
	}
	// 当前对话人全量。
	if !strings.Contains(joined, "怕坐飞机") {
		t.Error("current person memories missing")
	}
	// 原 system_prompt 兜底在底部。
	if !strings.Contains(joined, "全局兜底") {
		t.Error("original system prompt missing")
	}
	if !strings.Contains(joined, "不要罗列复述") {
		t.Error("不啰嗦 wording guard missing")
	}
	// 第一层 system 消息之后才接用户消息。
	if msgs[len(msgs)-1].Role != "user" {
		t.Error("last message should be the user message")
	}
}

func TestBuildMessagesPersona_LorebookTrigger(t *testing.T) {
	s := &mockPersonaStore{
		chars: []store.Character{{ID: "c1", Name: "小爱", CardJSON: cardJSON(t, "小爱", "", true)}},
		world: []store.WorldEntry{
			{ID: "w1", CharacterID: "c1", Keys: `["日本"]`, Content: "她下周去日本出差", Enabled: true},
			{ID: "w2", CharacterID: "c1", Keys: `["小猫"]`, Content: "她养了一只猫叫团子", Enabled: true},
		},
	}

	// Hit: 当前消息提到日本 → 注入日本条目，不注入小猫条目。
	cfg := store.AIConfig{CharacterID: "c1"}
	msgs := BuildMessagesPersona(context.Background(), cfg, s, "bot1", "ch1", "wxid_a", "我下周去日本", nil, nil)
	joined := joinSystem(msgs)
	if !strings.Contains(joined, "日本出差") {
		t.Error("lorebook entry for 日本 should be injected")
	}
	if strings.Contains(joined, "团子") {
		t.Error("unrelated lorebook entry must NOT be injected")
	}

	// No hit → no lorebook layer at all.
	msgs2 := BuildMessagesPersona(context.Background(), cfg, s, "bot1", "ch1", "wxid_a", "晚上吃啥", nil, nil)
	if strings.Contains(joinSystem(msgs2), "日本出差") {
		t.Error("lorebook should not inject without trigger")
	}
}

func TestBuildMessagesPersona_MemoryScopeCurrent(t *testing.T) {
	s := &mockPersonaStore{
		contacts: []store.Contact{
			{BotID: "bot1", Sender: "wxid_zhang", Name: "张三", Brief: "主人"},
			{BotID: "bot1", Sender: "wxid_li", Name: "李四", Brief: "张三女友"},
			{BotID: "bot1", Sender: "wxid_wang", Name: "王五", Brief: "同事"},
		},
		relations: []store.ContactRelation{
			{BotID: "bot1", FromSender: "wxid_zhang", ToSender: "wxid_li", Relation: "情侣"},
		},
	}
	cfg := store.AIConfig{MemoryScope: "current"}
	msgs := BuildMessagesPersona(context.Background(), cfg, s, "bot1", "ch1", "wxid_zhang", "hi", nil, nil)
	joined := joinSystem(msgs)
	if !strings.Contains(joined, "李四") {
		t.Error("current mode should include directly-connected person brief")
	}
	if strings.Contains(joined, "王五") {
		t.Error("current mode must NOT include unrelated person briefs")
	}

	// global mode includes everyone.
	cfg2 := store.AIConfig{MemoryScope: "global"}
	msgs2 := BuildMessagesPersona(context.Background(), cfg2, s, "bot1", "ch1", "wxid_zhang", "hi", nil, nil)
	if !strings.Contains(joinSystem(msgs2), "王五") {
		t.Error("global mode should include all briefs")
	}
}

func TestBuildMessagesPersona_FirstMesOnlyNewConversation(t *testing.T) {
	s := &mockPersonaStore{
		chars: []store.Character{{ID: "c1", Name: "小爱", CardJSON: cardJSONWithFirstMes(t, "小爱", "你好呀～")}},
	}
	cfg := store.AIConfig{CharacterID: "c1"}

	// New conversation (no history) → first_mes as assistant opening.
	msgs := BuildMessagesPersona(context.Background(), cfg, s, "bot1", "ch1", "wxid_a", "hi", nil, nil)
	found := false
	for _, m := range msgs {
		if m.Role == "assistant" {
			if c, ok := m.Content.(string); ok && c == "你好呀～" {
				found = true
			}
		}
	}
	if !found {
		t.Error("first_mes should appear for a new conversation")
	}
}

func TestBuildMessagesPersona_FallsBackToSenderHistoryWithoutChannel(t *testing.T) {
	s := &mockPersonaStore{
		chars: []store.Character{{ID: "c1", Name: "小爱", CardJSON: cardJSONWithFirstMes(t, "小爱", "你好呀～")}},
	}
	s.mockMessageStore.listMessagesBySender = []store.Message{
		{
			BotID:     "bot1",
			Direction: "outbound",
			ToUserID:  "wxid_a",
			ItemList:  mustJSONItems(t, []map[string]any{{"type": "text", "text": "上次聊过啦"}}),
		},
	}

	msgs := BuildMessagesPersona(context.Background(), store.AIConfig{CharacterID: "c1"}, s, "bot1", "", "wxid_a", "hi", nil, nil)

	if s.mockMessageStore.listChannelMessagesCalls != 0 {
		t.Fatalf("expected no channel history lookup, got %d calls", s.mockMessageStore.listChannelMessagesCalls)
	}
	if s.mockMessageStore.listMessagesBySenderCalls != 1 {
		t.Fatalf("expected sender history lookup once, got %d calls", s.mockMessageStore.listMessagesBySenderCalls)
	}

	var historySeen bool
	for _, m := range msgs {
		if m.Role == "assistant" {
			if c, ok := m.Content.(string); ok && c == "上次聊过啦" {
				historySeen = true
			}
			if c, ok := m.Content.(string); ok && c == "你好呀～" {
				t.Fatal("first_mes should not appear when sender history exists")
			}
		}
	}
	if !historySeen {
		t.Fatal("expected sender history to be included when channel id is empty")
	}
}

func TestBuildMessagesPersona_NoPersonaDegrades(t *testing.T) {
	s := &mockPersonaStore{}
	cfg := store.AIConfig{SystemPrompt: "仅系统提示"}
	msgs := BuildMessagesPersona(context.Background(), cfg, s, "bot1", "ch1", "wxid_a", "hi", nil, nil)
	// Only one system message, exactly the original prompt.
	sysCount := 0
	for _, m := range msgs {
		if m.Role == "system" {
			sysCount++
		}
	}
	if sysCount != 1 {
		t.Errorf("expected 1 system message when persona empty, got %d", sysCount)
	}
	if msgs[0].Role != "system" || msgs[0].Content.(string) != "仅系统提示" {
		t.Error("system prompt mismatch")
	}
}

func cardJSONWithFirstMes(t *testing.T, name, firstMes string) string {
	t.Helper()
	envelope := map[string]any{
		"spec": "chara_card_v2", "spec_version": "2.0",
		"data": map[string]any{"name": name, "first_mes": firstMes},
	}
	b, _ := json.Marshal(envelope)
	return string(b)
}

func joinSystem(msgs []Message) string {
	var b strings.Builder
	for _, m := range msgs {
		if m.Role == "system" {
			if c, ok := m.Content.(string); ok {
				b.WriteString(c)
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

func mustJSONItems(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}

func TestParseCard_CharacterBook(t *testing.T) {
	card, err := character.Parse([]byte(cardJSON(t, "小爱", "", true)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if card.Name != "小爱" {
		t.Errorf("name = %q", card.Name)
	}
	if len(card.WorldEntries) != 2 {
		t.Fatalf("world entries = %d, want 2", len(card.WorldEntries))
	}
	if card.WorldEntries[0].Keys[0] != "日本" {
		t.Errorf("first key = %q", card.WorldEntries[0].Keys[0])
	}
}

// ---------------------------------------------------------------------------
// Change 1: expires_at auto-forget — 过期记忆不注入组装上下文
// ---------------------------------------------------------------------------

func TestBuildMessagesPersona_ExpiredMemoriesExcluded(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Hour).Unix()
	s := &mockPersonaStore{
		contacts: []store.Contact{{BotID: "bot1", Sender: "wxid_a"}},
		memories: []store.Memory{
			{BotID: "bot1", Sender: "wxid_a", Category: "fact", Content: "已过期的事", Importance: 3, ExpiresAt: &past},
			{BotID: "bot1", Sender: "wxid_a", Category: "fact", Content: "还记得的事", Importance: 2},
		},
	}
	cfg := store.AIConfig{}
	msgs := BuildMessagesPersona(context.Background(), cfg, s, "bot1", "ch1", "wxid_a", "hi", nil, nil)
	joined := joinSystem(msgs)
	if strings.Contains(joined, "已过期的事") {
		t.Error("expired memory must not be injected into the persona context")
	}
	if !strings.Contains(joined, "还记得的事") {
		t.Error("non-expired memory should be injected into the persona context")
	}
}

// ---------------------------------------------------------------------------
// Change 2: 系统段总量护栏 + 分层截断
// ---------------------------------------------------------------------------

func TestTruncateRunes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"short unchanged", "hello", 10, "hello"},
		{"exact length unchanged", "abc", 3, "abc"},
		{"ascii truncated", "hello world", 5, "hello…"},
		{"multibyte not split", "中文测试文本", 3, "中文测…"},
		{"empty input", "", 5, ""},
		{"zero max", "abc", 0, "…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := truncateRunes(tc.in, tc.max); got != tc.want {
				t.Errorf("truncateRunes(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
		})
	}
}

func TestComposeCharacterLayer_TruncatesOversizedFields(t *testing.T) {
	long := strings.Repeat("甲", maxCardFieldRunes*2) // 4000 runes per field
	data := map[string]any{
		"name":        "小爱",
		"description": long,
		"personality": long,
		"scenario":    long,
	}
	envelope := map[string]any{"spec": "chara_card_v2", "spec_version": "2.0", "data": data}
	b, _ := json.Marshal(envelope)
	ch := &store.Character{ID: "c1", Name: "小爱", CardJSON: string(b)}

	got := composeCharacterLayer(ch, store.AIConfig{})
	if strings.Contains(got, "你是小爱") == false {
		t.Fatal("character persona should still be composed")
	}
	if n := strings.Count(got, "甲"); n != maxCardFieldRunes*3 {
		t.Errorf("card fields should each be truncated to %d runes, got %d total 甲", maxCardFieldRunes, n)
	}
	if n := strings.Count(got, "…"); n != 3 {
		t.Errorf("each truncated field should carry the ellipsis, got %d", n)
	}
}

func TestMatchWorldEntries_TruncatesOversizedContent(t *testing.T) {
	long := strings.Repeat("世", maxWorldEntryRunes*2) // 2000 runes
	entries := []store.WorldEntry{
		{ID: "w1", Keys: `["日本"]`, Content: long, Enabled: true},
		{ID: "w2", Keys: `["小猫"]`, Content: "她养了一只猫叫团子", Enabled: true},
	}
	got := matchWorldEntries(entries, "去日本看小猫")
	if n := strings.Count(got, "世"); n != maxWorldEntryRunes {
		t.Errorf("world entry content should be truncated to %d runes, got %d", maxWorldEntryRunes, n)
	}
	if !strings.Contains(got, "…") {
		t.Error("truncated entry should carry the ellipsis")
	}
	if !strings.Contains(got, "她养了一只猫叫团子") {
		t.Error("normal-size entry should be preserved verbatim")
	}
}

func TestBuildRelationSkeleton_CapsRelationsAndBriefs(t *testing.T) {
	var contacts []store.Contact
	contacts = append(contacts, store.Contact{BotID: "bot1", Sender: "wxid_0", Name: "零号", Brief: strings.Repeat("甲", maxBriefRunes*2)})
	var relations []store.ContactRelation
	for i := 0; i < maxRelationLines+5; i++ {
		relations = append(relations, store.ContactRelation{
			BotID:      "bot1",
			FromSender: fmt.Sprintf("wxid_%d", i),
			ToSender:   fmt.Sprintf("wxid_%d", i+1),
			Relation:   "朋友",
		})
	}
	s := &mockPersonaStore{contacts: contacts, relations: relations}
	got := buildRelationSkeleton(context.Background(), s, store.AIConfig{MemoryScope: "global"}, "bot1", "wxid_x", s.contacts)

	// brief 单行截断（"甲" 只出现在 brief 里，计数精确）。
	if n := strings.Count(got, "甲"); n != maxBriefRunes {
		t.Errorf("brief should be truncated to %d runes, got %d", maxBriefRunes, n)
	}
	if !strings.Contains(got, "…") {
		t.Error("oversized brief should be truncated with the ellipsis")
	}
	// 关系边封顶 + 省略标注。
	edgeLines := 0
	noteSeen := false
	for _, l := range strings.Split(got, "\n") {
		if strings.Contains(l, "—朋友—") {
			edgeLines++
		}
		if strings.Contains(l, "（关系较多，略）") {
			noteSeen = true
		}
	}
	if edgeLines > maxRelationLines {
		t.Errorf("relation edges = %d, want <= %d", edgeLines, maxRelationLines)
	}
	if !noteSeen {
		t.Error("expected the relation-omission note when edges exceed the cap")
	}
}

func TestBuildCurrentPerson_TruncatesOversizedMemoryInsteadOfDropping(t *testing.T) {
	// M5: 单条记忆超预算时截断保留，而不是整条丢弃。
	long := strings.Repeat("记", memoryBudgetRunes+2000)
	s := &mockPersonaStore{
		contacts: []store.Contact{{BotID: "bot1", Sender: "wxid_a"}},
		memories: []store.Memory{
			{BotID: "bot1", Sender: "wxid_a", Category: "fact", Content: long, Importance: 5},
		},
	}
	got := buildCurrentPerson(context.Background(), s, "bot1", "wxid_a", s.contacts)
	if !strings.Contains(got, "记") {
		t.Fatal("oversized single memory must be included (truncated), not dropped")
	}
	if n := strings.Count(got, "记"); n > memoryBudgetRunes {
		t.Errorf("memory content should be truncated to the %d-rune budget, got %d", memoryBudgetRunes, n)
	}
	if !strings.Contains(got, "…") {
		t.Error("truncated memory should carry the ellipsis")
	}
}

func TestComposeLayers_RulesNeverTruncatedUnderBudgetPressure(t *testing.T) {
	rules := strings.Repeat("规", 8000)
	l := &personaLayers{
		rules:     rules,
		character: strings.Repeat("角", 3000),
		lorebook:  strings.Repeat("书", 3000),
		relations: strings.Repeat("关", 2000),
		current:   strings.Repeat("忆", 1000),
	}
	out := composeLayers(l, strings.Repeat("系", 2000))
	joined := strings.Join(out, "\n")
	if n := strings.Count(joined, "规"); n != 8000 {
		t.Fatalf("rules layer must never be truncated, got %d of 8000", n)
	}
	// 超预算时其他层被收缩/丢弃来腾出空间，铁律层保持完整。
	if strings.Contains(joined, "角") || strings.Contains(joined, "系") {
		t.Error("character / system-prompt layers should be shrunk by the guardrail, rules kept")
	}
	if n := strings.Count(joined, "书"); n >= 3000 {
		t.Errorf("lorebook layer should be shrunk by the guardrail, got %d runes", n)
	}
	if total := len([]rune(strings.Join(out, ""))); total > maxSystemRunes {
		t.Errorf("total system runes = %d, want <= %d", total, maxSystemRunes)
	}
}

func TestComposeLayers_TotalBudgetTruncatesOversizedLayers(t *testing.T) {
	l := &personaLayers{
		character: strings.Repeat("角", 12000),
		current:   strings.Repeat("忆", 500),
	}
	out := composeLayers(l, strings.Repeat("系", 6000))
	joined := strings.Join(out, "\n")
	if total := len([]rune(strings.Join(out, ""))); total > maxSystemRunes {
		t.Errorf("total system runes = %d, want <= %d", total, maxSystemRunes)
	}
	// 自下而上：SystemPrompt 先被收缩（此处被整体丢弃），随后角色层被截断。
	if strings.Contains(joined, "系") {
		t.Error("system prompt layer should be shrunk first (bottom-up)")
	}
	if n := strings.Count(joined, "角"); n >= 12000 {
		t.Errorf("character layer should be truncated by the total guardrail, still %d runes", n)
	}
	if !strings.Contains(joined, "…") {
		t.Error("truncated character layer should carry the ellipsis")
	}
}

// ---------------------------------------------------------------------------
// Batch B: 关系层单一来源（M6+M7）— 每次组装仅一次 ListContacts
// ---------------------------------------------------------------------------

// countingPersonaStore wraps mockPersonaStore and counts the store round-trips
// that Batch B is supposed to dedupe.
type countingPersonaStore struct {
	*mockPersonaStore
	listContactsCalls int
	getCharacterCalls int
}

func (c *countingPersonaStore) ListContacts(botID string) ([]store.Contact, error) {
	c.listContactsCalls++
	return c.mockPersonaStore.ListContacts(botID)
}

func (c *countingPersonaStore) GetCharacter(id string) (*store.Character, error) {
	c.getCharacterCalls++
	return c.mockPersonaStore.GetCharacter(id)
}

func TestBuildMessagesPersona_SingleListContactsPerAssembly(t *testing.T) {
	s := &countingPersonaStore{
		mockPersonaStore: &mockPersonaStore{
			contacts: []store.Contact{
				{BotID: "bot1", Sender: "wxid_owner", Name: "主人", Brief: "唯一", IsOwner: true},
				{BotID: "bot1", Sender: "wxid_li", Name: "李四", Brief: "张三女友"},
			},
			relations: []store.ContactRelation{
				{BotID: "bot1", FromSender: "wxid_owner", ToSender: "wxid_li", Relation: "情侣"},
			},
			memories: []store.Memory{
				{BotID: "bot1", Sender: "wxid_owner", Category: "fact", Content: "怕坐飞机"},
			},
		},
	}
	cfg := store.AIConfig{MemoryScope: "global"}
	msgs := BuildMessagesPersona(context.Background(), cfg, s, "bot1", "ch1", "wxid_owner", "hi", nil, nil)
	joined := joinSystem(msgs)
	// 关系骨架 + 关系行 + 当前人层全部来自同一次 ListContacts 的结果。
	if !strings.Contains(joined, "主人") || !strings.Contains(joined, "怕坐飞机") {
		t.Fatal("persona layers should still be assembled")
	}
	if s.listContactsCalls != 1 {
		t.Fatalf("ListContacts called %d times per assembly, want 1", s.listContactsCalls)
	}
}

func TestBuildMessagesPersona_SingleGetCharacterPerAssembly(t *testing.T) {
	s := &countingPersonaStore{
		mockPersonaStore: &mockPersonaStore{
			chars: []store.Character{{ID: "c1", Name: "小爱", CardJSON: cardJSONWithFirstMes(t, "小爱", "你好呀～")}},
		},
	}
	cfg := store.AIConfig{CharacterID: "c1"}
	msgs := BuildMessagesPersona(context.Background(), cfg, s, "bot1", "ch1", "wxid_a", "hi", nil, nil)

	// first_mes 行为保持不变。
	found := false
	for _, m := range msgs {
		if m.Role == "assistant" {
			if c, ok := m.Content.(string); ok && c == "你好呀～" {
				found = true
			}
		}
	}
	if !found {
		t.Error("first_mes should still be injected for a new conversation")
	}
	if s.getCharacterCalls != 1 {
		t.Fatalf("GetCharacter called %d times per assembly, want 1", s.getCharacterCalls)
	}
}

func TestBuildMessagesPersona_RelationshipLineUsesDisplayName(t *testing.T) {
	s := &mockPersonaStore{
		contacts: []store.Contact{
			{BotID: "bot1", Sender: "wxid_owner", Name: "小主", IsOwner: true},
		},
	}
	cfg := store.AIConfig{}
	msgs := BuildMessagesPersona(context.Background(), cfg, s, "bot1", "ch1", "wxid_owner", "hi", nil, nil)
	joined := joinSystem(msgs)
	if !strings.Contains(joined, "当前联系人（小主）是你的主人/唯一喜欢的人。") {
		t.Errorf("relationship line should use display name 小主, got: %s", joined)
	}
	if strings.Contains(joined, "当前联系人（wxid_owner）") {
		t.Error("relationship line must not use the raw wxid when a name exists")
	}
}

func TestBuildCurrentPerson_OwnerLineOnlyInSkeleton(t *testing.T) {
	s := &mockPersonaStore{
		contacts: []store.Contact{
			{BotID: "bot1", Sender: "wxid_owner", Name: "小主", Brief: "唯一的喜欢的人", IsOwner: true},
		},
		memories: []store.Memory{
			{BotID: "bot1", Sender: "wxid_owner", Category: "fact", Content: "怕坐飞机"},
		},
	}
	// 当前人层不再重复 owner 行（owner 标记只在骨架一处）。
	got := buildCurrentPerson(context.Background(), s, "bot1", "wxid_owner", s.contacts)
	if strings.Contains(got, "主人") || strings.Contains(got, "喜欢的人") {
		t.Errorf("current-person layer must not carry the owner marker, got: %s", got)
	}
	if !strings.Contains(got, "怕坐飞机") {
		t.Error("current-person memories should still be injected")
	}
	// 骨架保留 owner 标记。
	sk := buildRelationSkeleton(context.Background(), s, store.AIConfig{MemoryScope: "global"}, "bot1", "wxid_owner", s.contacts)
	if !strings.Contains(sk, "主人/喜欢的人") {
		t.Error("relation skeleton must keep the owner marker")
	}
}

// ---------------------------------------------------------------------------
// Batch B: mes_example 注入（H4）
// ---------------------------------------------------------------------------

func TestComposeCharacterLayer_MesExampleInjected(t *testing.T) {
	data := map[string]any{
		"name":        "小爱",
		"description": "温柔的女友",
		"mes_example": "<START>\n小爱：你今天累吗？\n用户：累。\n小爱：那我陪你。",
	}
	envelope := map[string]any{"spec": "chara_card_v2", "spec_version": "2.0", "data": data}
	b, _ := json.Marshal(envelope)
	ch := &store.Character{ID: "c1", Name: "小爱", CardJSON: string(b)}

	got := composeCharacterLayer(ch, store.AIConfig{})
	if !strings.Contains(got, "对话风格示例") {
		t.Error("mes_example block label missing")
	}
	if !strings.Contains(got, "那我陪你") {
		t.Error("mes_example content should be injected into the character layer")
	}
}

func TestComposeCharacterLayer_MesExampleTruncated(t *testing.T) {
	// "演" 不出现在角色层其他文本（如「对话风格示例」标签）里，可精确计数。
	long := strings.Repeat("演", maxMesExampleRunes*2)
	data := map[string]any{"name": "小爱", "description": "x", "mes_example": long}
	envelope := map[string]any{"spec": "chara_card_v2", "spec_version": "2.0", "data": data}
	b, _ := json.Marshal(envelope)
	ch := &store.Character{ID: "c1", Name: "小爱", CardJSON: string(b)}

	got := composeCharacterLayer(ch, store.AIConfig{})
	if n := strings.Count(got, "演"); n != maxMesExampleRunes {
		t.Errorf("mes_example should be truncated to %d runes, got %d", maxMesExampleRunes, n)
	}
	if !strings.Contains(got, "…") {
		t.Error("truncated mes_example should carry the ellipsis")
	}
}

func TestComposeCharacterLayer_MesExampleOmittedWhenEmpty(t *testing.T) {
	ch := &store.Character{ID: "c1", Name: "小爱", CardJSON: cardJSON(t, "小爱", "温柔的女友", false)}
	got := composeCharacterLayer(ch, store.AIConfig{})
	if strings.Contains(got, "对话风格示例") {
		t.Error("mes_example block must be omitted when the card has none")
	}
}
