package persona

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/openilink/openilink-hub/internal/ai"
	"github.com/openilink/openilink-hub/internal/store"
)

// handleLookupContact pulls a person's full profile + memories + relations on
// demand (design §9.3: the lazy-loading bottom of the two-layer skeleton).
func handleLookupContact(ctx context.Context, e *Engine, botID, sender string, args map[string]any) ai.ToolCallResult {
	target := strArg(args, "sender")
	if target == "" {
		return ai.ToolCallResult{Content: "lookup_contact: sender required"}
	}
	var b strings.Builder

	if c, err := e.Store.GetContact(botID, target); err == nil && c != nil {
		if c.Name != "" {
			b.WriteString("名字/称呼：" + c.Name + "\n")
		}
		if c.Profile != "" {
			b.WriteString("画像：" + c.Profile + "\n")
		}
		if c.IsOwner {
			b.WriteString("标记：喜欢的人/主人\n")
		}
	} else {
		b.WriteString("（还没有这个人的画像记录）\n")
	}

	if mems, err := e.Store.ListMemories(botID, target); err == nil && len(mems) > 0 {
		b.WriteString("记忆：\n")
		now := time.Now().Unix()
		for _, m := range mems {
			if m.ExpiresAt != nil && *m.ExpiresAt <= now {
				continue // H1: expired — auto-forget (belt-and-braces over the SQL filter)
			}
			b.WriteString(fmt.Sprintf("- [%s] %s\n", m.Category, m.Content))
		}
	}

	if rels, err := e.Store.ListRelations(botID); err == nil && len(rels) > 0 {
		var lines []string
		for _, r := range rels {
			if r.FromSender == target || r.ToSender == target {
				lines = append(lines, fmt.Sprintf("%s —%s— %s", r.FromSender, r.Relation, r.ToSender))
			}
		}
		if len(lines) > 0 {
			b.WriteString("关系：\n" + strings.Join(lines, "\n") + "\n")
		}
	}

	return ai.ToolCallResult{Content: strings.TrimSpace(b.String())}
}

// handleUpsertContact evolves the "who this person is" profile.
func handleUpsertContact(ctx context.Context, e *Engine, botID, sender string, args map[string]any) ai.ToolCallResult {
	target := strArg(args, "sender")
	if target == "" {
		return ai.ToolCallResult{Content: "upsert_contact: sender required"}
	}
	existing, err := e.Store.GetContact(botID, target)
	if err != nil && err != store.ErrNotFound {
		return ai.ToolCallResult{Content: "upsert_contact failed"}
	}
	c := &store.Contact{BotID: botID, Sender: target}
	if existing != nil {
		c = existing
	}
	if v := strArg(args, "name"); v != "" {
		c.Name = v
	}
	if v := strArg(args, "brief"); v != "" {
		c.Brief = v
	}
	if v := strArg(args, "profile"); v != "" {
		c.Profile = v
	}
	if err := e.Store.UpsertContact(c); err != nil {
		return ai.ToolCallResult{Content: "upsert_contact failed"}
	}
	return ai.ToolCallResult{Content: "ok"}
}

// handleUpsertRelation records/updates a directed labelled edge.
func handleUpsertRelation(ctx context.Context, e *Engine, botID, sender string, args map[string]any) ai.ToolCallResult {
	from := strArg(args, "from_sender")
	to := strArg(args, "to_sender")
	relation := strArg(args, "relation")
	if from == "" || to == "" || relation == "" {
		return ai.ToolCallResult{Content: "upsert_relation: from_sender, to_sender, relation required"}
	}
	r := &store.ContactRelation{
		BotID:      botID,
		FromSender: from,
		ToSender:   to,
		Relation:   relation,
		Note:       strArg(args, "note"),
	}
	if err := e.Store.UpsertRelation(r); err != nil {
		return ai.ToolCallResult{Content: "upsert_relation failed"}
	}
	return ai.ToolCallResult{Content: "ok"}
}

// handleSetOwner marks "loved one / master"; at most one per bot.
func handleSetOwner(ctx context.Context, e *Engine, botID, sender string, args map[string]any) ai.ToolCallResult {
	target := strArg(args, "sender")
	if target == "" {
		return ai.ToolCallResult{Content: "set_owner: sender required"}
	}
	if err := e.Store.SetOwner(botID, target); err != nil {
		return ai.ToolCallResult{Content: "set_owner failed"}
	}
	return ai.ToolCallResult{Content: "ok"}
}
