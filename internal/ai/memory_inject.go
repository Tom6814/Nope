package ai

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/openilink/openilink-hub/internal/store"
)

// buildRelationSkeleton renders layer [C]: the relationship-tree skeleton.
// Only cheap data is resident here (one brief per contact + edges); full
// profiles are loaded on demand via the lookup_contact tool.
//
// contacts is the already-fetched contact list for botID (buildPersonaLayers
// calls ListContacts once and shares the result — one query per assembly).
//
// memory_scope differences:
//   - "global"  (default): all contacts' briefs + full relation tree.
//   - "current": only edges touching the current sender + directly connected
//     briefs.
//   - "freeform": empty skeleton — the AI grows the tree itself via tools.
func buildRelationSkeleton(ctx context.Context, s PersonaStore, cfg store.AIConfig, botID, sender string, contacts []store.Contact) string {
	scope := cfg.MemoryScope
	if scope == "" {
		scope = "global"
	}

	relations, _ := s.ListRelations(botID)
	bySender := make(map[string]store.Contact, len(contacts))
	for _, c := range contacts {
		bySender[c.Sender] = c
	}

	var lines []string

	switch scope {
	case "current":
		// Edges touching the current sender, plus briefs of directly connected
		// contacts only.
		related := map[string]bool{sender: true}
		for _, r := range relations {
			if r.FromSender == sender {
				related[r.ToSender] = true
			}
			if r.ToSender == sender {
				related[r.FromSender] = true
			}
		}
		var edgeLines []string
		for _, r := range relations {
			if r.FromSender == sender || r.ToSender == sender {
				edgeLines = append(edgeLines, formatRelation(r, bySender))
			}
		}
		lines = append(lines, capRelationLines(edgeLines)...)
		for _, c := range contacts {
			if related[c.Sender] && c.Brief != "" {
				lines = append(lines, fmt.Sprintf("%s：%s", displaySender(c), truncateRunes(c.Brief, maxBriefRunes)))
			}
		}
	case "freeform":
		return ""
	default: // global
		for _, c := range contacts {
			if c.Brief == "" {
				continue
			}
			marker := ""
			if c.IsOwner {
				marker = "（主人/喜欢的人）"
			}
			lines = append(lines, fmt.Sprintf("%s%s：%s", displaySender(c), marker, truncateRunes(c.Brief, maxBriefRunes)))
			if len(lines) >= relationBudgetLines {
				break
			}
		}
		var edgeLines []string
		for _, r := range relations {
			edgeLines = append(edgeLines, formatRelation(r, bySender))
		}
		lines = append(lines, capRelationLines(edgeLines)...)
	}

	if len(lines) == 0 {
		return ""
	}

	// "不生硬" 措辞框定 (design §5.2): this is what the AI knows, not what it
	// must recite.
	return "【你认识的人（心里有数即可，是否提及、提多少，凭你的性格与分寸，不要随便暴露别人的秘密）】：\n" + strings.Join(lines, "\n")
}

// buildCurrentPerson renders layer [D]: the full picture of the person being
// talked to right now — always fully resident for the active sender. contacts
// is the already-fetched contact list (shared with the skeleton and the
// relationship-context line); the owner marker lives in the skeleton only.
func buildCurrentPerson(ctx context.Context, s PersonaStore, botID, sender string, contacts []store.Contact) string {
	var parts []string

	for i := range contacts {
		if contacts[i].Sender != sender {
			continue
		}
		c := &contacts[i]
		if c.Name != "" {
			parts = append(parts, "这位的名字/称呼："+c.Name)
		}
		if c.Profile != "" {
			parts = append(parts, "画像："+c.Profile)
		}
		break
	}

	mems, _ := s.ListMemories(botID, sender)
	var memLines []string
	total := 0
	now := time.Now().Unix()
	// ListMemories returns importance-first; budget the total length by runes.
	// A single memory that exceeds the remaining budget is truncated and kept
	// rather than dropped entirely (M5).
	for _, m := range mems {
		if m.ExpiresAt != nil && *m.ExpiresAt <= now {
			continue // H1: expired — auto-forget (belt-and-braces over the SQL filter)
		}
		line := fmt.Sprintf("[%s%s] %s", m.Category, importanceTag(m.Importance), m.Content)
		lineRunes := len([]rune(line))
		if total+lineRunes <= memoryBudgetRunes {
			memLines = append(memLines, line)
			total += lineRunes
			continue
		}
		if total < memoryBudgetRunes {
			memLines = append(memLines, truncateRunes(line, memoryBudgetRunes-total))
		}
		break
	}
	if len(memLines) > 0 {
		parts = append(parts, "你记得的这些事：\n"+strings.Join(memLines, "\n"))
	}

	if len(parts) == 0 {
		return ""
	}
	// 措辞框定: express naturally, don't recite a list.
	return "【以下是你对正在聊天这位的了解，自然体现在你的态度、称呼和用词里，不要罗列复述，也不要说「我记得你说过…」之类的话】：\n" + strings.Join(parts, "\n")
}

func displaySender(c store.Contact) string {
	if c.Name != "" {
		return c.Name
	}
	return c.Sender
}

// capRelationLines caps relationship-edge lines at maxRelationLines, appending
// an omission note when edges were cut.
func capRelationLines(edgeLines []string) []string {
	if len(edgeLines) <= maxRelationLines {
		return edgeLines
	}
	return append(edgeLines[:maxRelationLines], "（关系较多，略）")
}

// formatRelation renders one directed labelled edge. Endpoint senders are
// shown with their display name when known (bySender), falling back to the
// raw wxid — so edges read like the rest of the skeleton instead of leaking
// internal ids (M7).
func formatRelation(r store.ContactRelation, bySender map[string]store.Contact) string {
	from := r.FromSender
	if c, ok := bySender[r.FromSender]; ok {
		from = displaySender(c)
	}
	to := r.ToSender
	if c, ok := bySender[r.ToSender]; ok {
		to = displaySender(c)
	}
	label := fmt.Sprintf("%s —%s— %s", from, r.Relation, to)
	if r.Note != "" {
		label += "（" + r.Note + "）"
	}
	return label
}

func importanceTag(i int) string {
	if i >= 4 {
		return "*"
	}
	return ""
}
