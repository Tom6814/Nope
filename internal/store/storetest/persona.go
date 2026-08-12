package storetest

import (
	"strings"
	"testing"
	"time"

	"github.com/openilink/openilink-hub/internal/store"
)

// TestCharacterCRUD exercises character cards and world entries.
func TestCharacterCRUD(t *testing.T, s store.Store) {
	t.Run("SaveAndGetCharacter", func(t *testing.T) {
		c := &store.Character{Name: "Alice", Spec: "chara_card_v2", CardJSON: `{"data":{"name":"Alice"}}`}
		if err := s.SaveCharacter(c); err != nil {
			t.Fatalf("SaveCharacter: %v", err)
		}
		if c.ID == "" {
			t.Fatal("SaveCharacter should assign an ID")
		}
		got, err := s.GetCharacter(c.ID)
		if err != nil {
			t.Fatalf("GetCharacter: %v", err)
		}
		if got.Name != "Alice" {
			t.Errorf("name = %q, want Alice", got.Name)
		}
	})

	t.Run("UpdateCharacter", func(t *testing.T) {
		chars, _ := s.ListCharacters()
		if len(chars) == 0 {
			t.Fatal("expected at least one character")
		}
		chars[0].CardJSON = `{"data":{"name":"Alice2"}}`
		if err := s.SaveCharacter(&chars[0]); err != nil {
			t.Fatalf("SaveCharacter(update): %v", err)
		}
		got, _ := s.GetCharacter(chars[0].ID)
		if got.CardJSON != chars[0].CardJSON {
			t.Error("card_json was not updated")
		}
	})

	t.Run("GetCharacter_NotFound", func(t *testing.T) {
		_, err := s.GetCharacter("nonexistent")
		if err == nil {
			t.Error("expected error for non-existent character")
		}
	})

	t.Run("WorldEntries", func(t *testing.T) {
		chars, _ := s.ListCharacters()
		charID := chars[0].ID

		// Global entry
		ge := &store.WorldEntry{Keys: `["global"]`, Content: "global lore", Enabled: true, Priority: 1}
		if err := s.SaveWorldEntry(ge); err != nil {
			t.Fatalf("SaveWorldEntry(global): %v", err)
		}
		// Character entry
		ce := &store.WorldEntry{CharacterID: charID, Keys: `["alice"]`, Content: "alice lore", Enabled: true}
		if err := s.SaveWorldEntry(ce); err != nil {
			t.Fatalf("SaveWorldEntry(char): %v", err)
		}

		// ListWorldEntries("") → global only
		globalOnly, err := s.ListWorldEntries("")
		if err != nil {
			t.Fatalf("ListWorldEntries(global): %v", err)
		}
		for _, e := range globalOnly {
			if e.CharacterID != "" {
				t.Errorf("expected only global entries, got character entry %q", e.ID)
			}
		}
		if len(globalOnly) < 1 {
			t.Error("expected at least one global entry")
		}

		// ListWorldEntries(charID) → char + global
		all, err := s.ListWorldEntries(charID)
		if err != nil {
			t.Fatalf("ListWorldEntries(char): %v", err)
		}
		if len(all) < 2 {
			t.Errorf("expected >= 2 entries (char+global), got %d", len(all))
		}
	})

	t.Run("DeleteCharacter", func(t *testing.T) {
		c := &store.Character{Name: "ToDelete", CardJSON: `{}`}
		s.SaveCharacter(c)
		if err := s.DeleteCharacter(c.ID); err != nil {
			t.Fatalf("DeleteCharacter: %v", err)
		}
		if _, err := s.GetCharacter(c.ID); err == nil {
			t.Error("expected error after deleting character")
		}
		// World entries for this character should cascade
		if _, err := s.ListWorldEntries(c.ID); err != nil {
			t.Errorf("ListWorldEntries after delete: %v", err)
		}
	})
}

// TestContactCRUD exercises the relationship tree (contacts + relations).
func TestContactCRUD(t *testing.T, s store.Store) {
	u := mustCreateUser(t, s, "contactowner", "Contact Owner")
	b := mustCreateBot(t, s, u.ID, "ContactBot")

	t.Run("UpsertAndGet", func(t *testing.T) {
		c := &store.Contact{BotID: b.ID, Sender: "wxid_zhang", Name: "张三", Brief: "主人", Profile: "程序员，最近换工作"}
		if err := s.UpsertContact(c); err != nil {
			t.Fatalf("UpsertContact: %v", err)
		}
		got, err := s.GetContact(b.ID, "wxid_zhang")
		if err != nil {
			t.Fatalf("GetContact: %v", err)
		}
		if got.Name != "张三" || got.Brief != "主人" {
			t.Errorf("contact mismatch: %+v", got)
		}
	})

	t.Run("UpsertByIdempotentOnScope", func(t *testing.T) {
		// Same (bot, sender) → upsert not duplicate
		if err := s.UpsertContact(&store.Contact{BotID: b.ID, Sender: "wxid_zhang", Name: "张三", Brief: "主人，换了新工作", Profile: "新画像"}); err != nil {
			t.Fatalf("UpsertContact(update): %v", err)
		}
		contacts, _ := s.ListContacts(b.ID)
		count := 0
		for _, c := range contacts {
			if c.Sender == "wxid_zhang" {
				count++
			}
		}
		if count != 1 {
			t.Errorf("expected 1 contact for wxid_zhang, got %d", count)
		}
		got, _ := s.GetContact(b.ID, "wxid_zhang")
		if got.Profile != "新画像" {
			t.Errorf("profile = %q, want 新画像", got.Profile)
		}
	})

	t.Run("ListContacts", func(t *testing.T) {
		s.UpsertContact(&store.Contact{BotID: b.ID, Sender: "wxid_li", Name: "李四", Brief: "张三女友"})
		contacts, err := s.ListContacts(b.ID)
		if err != nil {
			t.Fatalf("ListContacts: %v", err)
		}
		if len(contacts) < 2 {
			t.Errorf("expected >= 2 contacts, got %d", len(contacts))
		}
	})

	t.Run("GetContact_NotFound", func(t *testing.T) {
		_, err := s.GetContact(b.ID, "wxid_nobody")
		if err == nil {
			t.Error("expected error for non-existent contact")
		}
	})

	t.Run("SetOwnerUnique", func(t *testing.T) {
		s.UpsertContact(&store.Contact{BotID: b.ID, Sender: "wxid_zhang"})
		s.UpsertContact(&store.Contact{BotID: b.ID, Sender: "wxid_li"})
		if err := s.SetOwner(b.ID, "wxid_zhang"); err != nil {
			t.Fatalf("SetOwner: %v", err)
		}
		if err := s.SetOwner(b.ID, "wxid_li"); err != nil {
			t.Fatalf("SetOwner(2): %v", err)
		}
		contacts, _ := s.ListContacts(b.ID)
		owners := 0
		for _, c := range contacts {
			if c.IsOwner {
				owners++
			}
		}
		if owners != 1 {
			t.Errorf("expected exactly 1 owner, got %d", owners)
		}
	})

	t.Run("Relations", func(t *testing.T) {
		r := &store.ContactRelation{BotID: b.ID, FromSender: "wxid_zhang", ToSender: "wxid_li", Relation: "情侣", Note: "2026 在一起"}
		if err := s.UpsertRelation(r); err != nil {
			t.Fatalf("UpsertRelation: %v", err)
		}
		rels, err := s.ListRelations(b.ID)
		if err != nil {
			t.Fatalf("ListRelations: %v", err)
		}
		if len(rels) < 1 {
			t.Fatal("expected at least 1 relation")
		}
		if rels[0].Relation != "情侣" {
			t.Errorf("relation = %q, want 情侣", rels[0].Relation)
		}
		if err := s.DeleteRelation(r.ID); err != nil {
			t.Fatalf("DeleteRelation: %v", err)
		}
		rels, _ = s.ListRelations(b.ID)
		for _, rr := range rels {
			if rr.ID == r.ID {
				t.Error("relation should have been deleted")
			}
		}
	})

	t.Run("DeleteContact", func(t *testing.T) {
		s.UpsertContact(&store.Contact{BotID: b.ID, Sender: "wxid_tmp"})
		if err := s.DeleteContact(b.ID, "wxid_tmp"); err != nil {
			t.Fatalf("DeleteContact: %v", err)
		}
		if _, err := s.GetContact(b.ID, "wxid_tmp"); err == nil {
			t.Error("expected error after deleting contact")
		}
	})
}

// TestMemoryCRUD exercises memories with (bot, sender) isolation.
func TestMemoryCRUD(t *testing.T, s store.Store) {
	u := mustCreateUser(t, s, "memowner", "Mem Owner")
	b := mustCreateBot(t, s, u.ID, "MemBot")

	t.Run("AddAndList", func(t *testing.T) {
		m := &store.Memory{BotID: b.ID, Sender: "wxid_a", Category: "profile", Content: "怕坐飞机", Importance: 3}
		if err := s.AddMemory(m); err != nil {
			t.Fatalf("AddMemory: %v", err)
		}
		got, err := s.ListMemories(b.ID, "wxid_a")
		if err != nil {
			t.Fatalf("ListMemories: %v", err)
		}
		if len(got) != 1 || got[0].Content != "怕坐飞机" {
			t.Errorf("memory mismatch: %+v", got)
		}
	})

	t.Run("SenderIsolation", func(t *testing.T) {
		// Same bot, different sender must not cross.
		s.AddMemory(&store.Memory{BotID: b.ID, Sender: "wxid_b", Content: "李四的秘密", Importance: 2})
		a, _ := s.ListMemories(b.ID, "wxid_a")
		for _, m := range a {
			if m.Sender != "wxid_a" {
				t.Errorf("cross-sender leak: %+v", m)
			}
		}
	})

	t.Run("CrossChannelMerge", func(t *testing.T) {
		// No channel dimension: same (bot, sender) in any channel = same stream.
		s.AddMemory(&store.Memory{BotID: b.ID, Sender: "wxid_a", Content: "下周去日本出差", Category: "fact", Importance: 2})
		got, _ := s.ListMemories(b.ID, "wxid_a")
		if len(got) != 2 {
			t.Errorf("expected 2 memories for wxid_a (cross-channel merge), got %d", len(got))
		}
	})

	t.Run("Count", func(t *testing.T) {
		n, err := s.CountMemories(b.ID, "wxid_a")
		if err != nil {
			t.Fatalf("CountMemories: %v", err)
		}
		if n != 2 {
			t.Errorf("count = %d, want 2", n)
		}
	})

	t.Run("ListMemoriesByBot", func(t *testing.T) {
		// Covers all senders of a bot in one query, scoped to this bot only.
		got, err := s.ListMemoriesByBot(b.ID)
		if err != nil {
			t.Fatalf("ListMemoriesByBot: %v", err)
		}
		bySender := map[string]int{}
		for _, m := range got {
			if m.BotID != b.ID {
				t.Errorf("memory %s leaked from another bot: %+v", m.ID, m)
			}
			bySender[m.Sender]++
		}
		if bySender["wxid_a"] != 2 {
			t.Errorf("wxid_a memories = %d, want 2", bySender["wxid_a"])
		}
		if bySender["wxid_b"] != 1 {
			t.Errorf("wxid_b memories = %d, want 1", bySender["wxid_b"])
		}
		// Isolation: a different bot must not see these memories.
		b2 := mustCreateBot(t, s, u.ID, "MemBot2")
		other, err := s.ListMemoriesByBot(b2.ID)
		if err != nil {
			t.Fatalf("ListMemoriesByBot(other bot): %v", err)
		}
		if len(other) != 0 {
			t.Errorf("expected 0 memories for other bot, got %d", len(other))
		}
	})

	t.Run("UpdateAndDelete", func(t *testing.T) {
		got, _ := s.ListMemories(b.ID, "wxid_b")
		if len(got) == 0 {
			t.Fatal("expected memory for wxid_b")
		}
		m := got[0]
		m.Content = "更新后的内容"
		m.Importance = 5
		if err := s.UpdateMemory(&m); err != nil {
			t.Fatalf("UpdateMemory: %v", err)
		}
		got2, _ := s.ListMemories(b.ID, "wxid_b")
		if got2[0].Content != "更新后的内容" || got2[0].Importance != 5 {
			t.Errorf("memory not updated: %+v", got2[0])
		}
		if err := s.DeleteMemory(m.ID); err != nil {
			t.Fatalf("DeleteMemory: %v", err)
		}
		got3, _ := s.ListMemories(b.ID, "wxid_b")
		if len(got3) != 0 {
			t.Errorf("expected 0 memories after delete, got %d", len(got3))
		}
	})

	t.Run("ExpiredMemoriesFiltered", func(t *testing.T) {
		// H1: expires_at auto-forget — expired memories must not be returned by
		// ListMemories; nil/future expiry keeps the memory alive.
		now := time.Now()
		past := now.Add(-time.Hour).Unix()
		future := now.Add(24 * time.Hour).Unix()
		expired := &store.Memory{BotID: b.ID, Sender: "wxid_exp", Category: "fact", Content: "过期的秘密", Importance: 3, ExpiresAt: &past}
		live := &store.Memory{BotID: b.ID, Sender: "wxid_exp", Category: "fact", Content: "未来的约定", Importance: 2, ExpiresAt: &future}
		never := &store.Memory{BotID: b.ID, Sender: "wxid_exp", Category: "fact", Content: "永久记忆", Importance: 1}
		for _, m := range []*store.Memory{expired, live, never} {
			if err := s.AddMemory(m); err != nil {
				t.Fatalf("AddMemory(%q): %v", m.Content, err)
			}
		}
		got, err := s.ListMemories(b.ID, "wxid_exp")
		if err != nil {
			t.Fatalf("ListMemories: %v", err)
		}
		var contents []string
		for _, m := range got {
			contents = append(contents, m.Content)
		}
		joined := strings.Join(contents, "|")
		if strings.Contains(joined, "过期的秘密") {
			t.Errorf("expired memory must NOT be returned by ListMemories, got %v", contents)
		}
		if !strings.Contains(joined, "未来的约定") || !strings.Contains(joined, "永久记忆") {
			t.Errorf("nil/future-expiry memories should be returned, got %v", contents)
		}
	})
}

// TestScheduledMessageCRUD exercises proactive/promise message scheduling.
func TestScheduledMessageCRUD(t *testing.T, s store.Store) {
	u := mustCreateUser(t, s, "schedowner", "Sched Owner")
	b := mustCreateBot(t, s, u.ID, "SchedBot")

	t.Run("AddAndDue", func(t *testing.T) {
		past := int64(1000)
		future := int64(1_000_000_000)
		due := &store.ScheduledMessage{BotID: b.ID, Sender: "wxid_a", FireAt: past, Tolerance: 120, Prompt: "叫主人起床", Origin: "promise"}
		notDue := &store.ScheduledMessage{BotID: b.ID, Sender: "wxid_a", FireAt: future, Tolerance: 600, Prompt: "明天再说", Origin: "proactive"}
		if err := s.AddScheduledMessage(due); err != nil {
			t.Fatalf("AddScheduledMessage(due): %v", err)
		}
		if err := s.AddScheduledMessage(notDue); err != nil {
			t.Fatalf("AddScheduledMessage(notDue): %v", err)
		}

		list, err := s.ListDueScheduledMessages(int64(2000))
		if err != nil {
			t.Fatalf("ListDueScheduledMessages: %v", err)
		}
		if len(list) != 1 || list[0].ID != due.ID {
			t.Errorf("expected only the due message, got %d entries", len(list))
		}
	})

	t.Run("MarkSent", func(t *testing.T) {
		list, _ := s.ListDueScheduledMessages(int64(2000))
		if len(list) == 0 {
			t.Fatal("expected a due message")
		}
		if err := s.MarkScheduledMessageSent(list[0].ID, int64(1500)); err != nil {
			t.Fatalf("MarkScheduledMessageSent: %v", err)
		}
		// No longer due after sent
		after, _ := s.ListDueScheduledMessages(int64(2000))
		for _, m := range after {
			if m.ID == list[0].ID {
				t.Error("sent message should not be listed as due")
			}
		}
	})

	t.Run("MarkFailedAndCancel", func(t *testing.T) {
		m := &store.ScheduledMessage{BotID: b.ID, Sender: "wxid_b", FireAt: 100, Prompt: "x", Origin: "promise"}
		s.AddScheduledMessage(m)
		if err := s.MarkScheduledMessageFailed(m.ID, "会话窗口已过期"); err != nil {
			t.Fatalf("MarkScheduledMessageFailed: %v", err)
		}
		m2 := &store.ScheduledMessage{BotID: b.ID, Sender: "wxid_b", FireAt: 100, Prompt: "y", Origin: "proactive"}
		s.AddScheduledMessage(m2)
		if err := s.CancelScheduledMessage(m2.ID); err != nil {
			t.Fatalf("CancelScheduledMessage: %v", err)
		}
		due, _ := s.ListDueScheduledMessages(200)
		for _, mm := range due {
			if mm.ID == m.ID || mm.ID == m2.ID {
				t.Errorf("failed/cancelled message %s should not be due", mm.ID)
			}
		}
	})

	t.Run("ClaimOnlyOnce", func(t *testing.T) {
		m := &store.ScheduledMessage{BotID: b.ID, Sender: "wxid_c", FireAt: 120, Prompt: "claim me", Origin: "promise"}
		if err := s.AddScheduledMessage(m); err != nil {
			t.Fatalf("AddScheduledMessage(claim): %v", err)
		}

		ok, err := s.ClaimScheduledMessage(m.ID)
		if err != nil {
			t.Fatalf("ClaimScheduledMessage(first): %v", err)
		}
		if !ok {
			t.Fatal("first claim should succeed")
		}

		ok, err = s.ClaimScheduledMessage(m.ID)
		if err != nil {
			t.Fatalf("ClaimScheduledMessage(second): %v", err)
		}
		if ok {
			t.Fatal("second claim should fail once the message is already claimed")
		}

		due, err := s.ListDueScheduledMessages(200)
		if err != nil {
			t.Fatalf("ListDueScheduledMessages(after claim): %v", err)
		}
		for _, mm := range due {
			if mm.ID == m.ID {
				t.Fatalf("claimed message %s should no longer appear as due", mm.ID)
			}
		}
	})

	t.Run("ListScheduledMessages", func(t *testing.T) {
		all, err := s.ListScheduledMessages(b.ID, 10)
		if err != nil {
			t.Fatalf("ListScheduledMessages: %v", err)
		}
		if len(all) < 3 {
			t.Errorf("expected >= 3 scheduled messages, got %d", len(all))
		}
	})

	t.Run("RescheduleCountRoundTrip", func(t *testing.T) {
		m := &store.ScheduledMessage{
			BotID:           b.ID,
			Sender:          "wxid_d",
			FireAt:          int64(time.Now().Add(60 * time.Second).Unix()),
			Prompt:          "x",
			Origin:          "proactive",
			Status:          "pending",
			RescheduleCount: 2,
		}
		if err := s.AddScheduledMessage(m); err != nil {
			t.Fatalf("AddScheduledMessage: %v", err)
		}
		got, err := s.ListPendingByScope(b.ID, "wxid_d", "proactive")
		if err != nil {
			t.Fatalf("ListPendingByScope: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("expected 1 pending scheduled message, got %d", len(got))
		}
		if got[0].RescheduleCount != 2 {
			t.Errorf("reschedule_count = %d, want 2", got[0].RescheduleCount)
		}
	})
}

// TestPersonaRuleCRUD exercises the three-level persona rules fallback.
func TestPersonaRuleCRUD(t *testing.T, s store.Store) {
	t.Run("DefaultFallback", func(t *testing.T) {
		r, err := s.GetPersonaRules("bot_nonexistent")
		if err != nil {
			t.Fatalf("GetPersonaRules(default): %v", err)
		}
		if !r.Enabled {
			t.Error("default rules should be enabled")
		}
		if r.RulesText == "" {
			t.Error("default rules should have text")
		}
	})

	t.Run("SaveAndGetGlobal", func(t *testing.T) {
		r := &store.PersonaRules{ID: "default", BotID: "", Enabled: true, RulesText: "测试铁律"}
		if err := s.SavePersonaRules(r); err != nil {
			t.Fatalf("SavePersonaRules: %v", err)
		}
		got, err := s.GetPersonaRules("any_bot")
		if err != nil {
			t.Fatalf("GetPersonaRules(global fallback): %v", err)
		}
		if got.RulesText != "测试铁律" {
			t.Errorf("rules_text = %q, want 测试铁律", got.RulesText)
		}
		if !got.Enabled {
			t.Error("expected enabled")
		}
	})

	t.Run("Disable", func(t *testing.T) {
		got, _ := s.GetPersonaRules("any_bot")
		got.Enabled = false
		if err := s.SavePersonaRules(got); err != nil {
			t.Fatalf("SavePersonaRules(disable): %v", err)
		}
		got2, _ := s.GetPersonaRules("any_bot")
		if got2.Enabled {
			t.Error("expected disabled")
		}
		// Restore for other tests
		got2.Enabled = true
		s.SavePersonaRules(got2)
	})
}
