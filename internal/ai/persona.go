package ai

import (
	"context"
	"strings"

	"github.com/openilink/openilink-hub/internal/character"
	"github.com/openilink/openilink-hub/internal/store"
)

// PersonaStore bundles the stores needed to assemble the persona system layers.
// store.Store satisfies this interface, so the AI sink can pass its store
// directly.
type PersonaStore interface {
	store.MessageStore
	store.CharacterStore
	store.ContactStore
	store.MemoryStore
	store.PersonaRuleStore
}

// personaLayers holds the assembled system-segment layers in fixed order:
// rules (top) → relationship context → character → lorebook → relation
// skeleton → current person. char is the fetched character card, carried so
// loadFirstMes can reuse it instead of issuing a second GetCharacter (H3).
type personaLayers struct {
	rules        string
	relationship string
	character    string
	lorebook     string
	relations    string
	current      string
	char         *store.Character
}

// memoryBudgetRunes caps how much of a single person's memories is injected to
// protect the context window. Measured in runes (not bytes) so CJK text is not
// penalized ~3x; a single oversized memory is truncated to the remaining
// budget instead of being dropped (M5).
const memoryBudgetRunes = 3000

// relationBudget caps how many contact briefs are resident in context.
const relationBudgetLines = 100

// buildPersonaLayers assembles every persona layer for a conversation.
// botID is passed in by the caller (derived upstream from the channel).
// One assembly issues exactly one GetCharacter and one ListContacts; the
// results are shared across the layers that need them (H3).
func buildPersonaLayers(ctx context.Context, cfg store.AIConfig, s PersonaStore, botID, sender, text string) *personaLayers {
	layers := &personaLayers{}

	// [铁律] persona rules — top priority, unaffected by colloquial rewriting.
	if rules, err := s.GetPersonaRules(botID); err == nil && rules != nil && rules.Enabled {
		text := strings.TrimSpace(rules.RulesText)
		if text != "" {
			layers.rules = text
		}
	}

	// [A] 角色卡人格层.
	if cfg.CharacterID != "" {
		if ch, err := s.GetCharacter(cfg.CharacterID); err == nil && ch != nil {
			layers.char = ch // reused by loadFirstMes — no second GetCharacter.
			layers.character = composeCharacterLayer(ch, cfg)
		}
	}

	// [B] 世界书层 — keyword-triggered on recent text.
	if cfg.CharacterID != "" {
		if entries, err := s.ListWorldEntries(cfg.CharacterID); err == nil && len(entries) > 0 {
			layers.lorebook = matchWorldEntries(entries, text)
		}
	}

	// [C] 关系树骨架 + [D] 当前对话人全量 + 当前联系人角色行 — 共享同一次
	// ListContacts 的结果（M6/M7）。
	if botID != "" {
		contacts, _ := s.ListContacts(botID)
		layers.relations = buildRelationSkeleton(ctx, s, cfg, botID, sender, contacts)
		layers.current = buildCurrentPerson(ctx, s, botID, sender, contacts)
		// 当前联系人角色（owner / 普通联系人 / 陌生人），让 AI 知道在对谁说话。
		layers.relationship = store.RelationshipContextWith(contacts, sender)
	}

	return layers
}

// composeCharacterLayer renders layer [A]: "你是<name>，…" plus the card's own
// system_prompt / post_history_instructions, a truncated dialogue-style example
// (mes_example, H4) and the WeChat colloquial style. Oversized card fields are
// truncated per-field (maxCardFieldRunes).
func composeCharacterLayer(ch *store.Character, cfg store.AIConfig) string {
	var parts []string

	// Parse the card (best-effort; a corrupt card degrades to no persona).
	if card, err := parseCard(ch.CardJSON); err == nil && card != nil {
		card.Description = truncateRunes(card.Description, maxCardFieldRunes)
		card.Personality = truncateRunes(card.Personality, maxCardFieldRunes)
		card.Scenario = truncateRunes(card.Scenario, maxCardFieldRunes)
		if p := character.ComposePersonaText(card); p != "" {
			parts = append(parts, p)
		}
		// 对话风格示例：让模型模仿角色的说话风格（仅非空时注入，截断到
		// maxMesExampleRunes）。
		if card.MesExample != "" {
			parts = append(parts, "（对话风格示例）\n"+truncateRunes(card.MesExample, maxMesExampleRunes))
		}
		if card.SystemPrompt != "" {
			parts = append(parts, card.SystemPrompt)
		}
		if card.PostHistoryInstructions != "" {
			parts = append(parts, card.PostHistoryInstructions)
		}
	}

	// 微信口语体 (design §10.1) — appended to the persona layer.
	if cfg.HumanizeWechat {
		parts = append(parts, wechatStyleInstruction)
	}

	return strings.Join(parts, "\n\n")
}

// parseCard parses a stored card JSON, returning nil on any error.
func parseCard(cardJSON string) (*character.Card, error) {
	return character.Parse([]byte(cardJSON))
}

const wechatStyleInstruction = `你在用微信聊天，像真人一样：
- 句尾不加句号（问号 / 感叹号在表达情绪时才用）
- 一次想说的话拆成几条短句，用换行分隔，不要堆成一大段
- 偶尔可以单独一条只发一个字或一个标点（「？」「哈哈哈」「嗯…」）
- 不用书面语和 markdown，不列 1. 2. 3.`

// composeLayers joins non-empty layers with markers into one system block.
// Each layer is wrapped with an explicit instruction so the model treats it as
// background knowledge, not spoken lines. The rules layer is exempt from the
// total-budget guardrail; other layers are shrunk bottom-up (system prompt →
// character → world book → relation skeleton) only when needed.
func composeLayers(l *personaLayers, systemPrompt string) []string {
	var layers []systemLayer
	if l.rules != "" {
		layers = append(layers, systemLayer{text: "【铁律】必须遵守，这是约束不是台词：\n" + l.rules, fixed: true})
	}
	if l.relationship != "" {
		layers = append(layers, systemLayer{text: l.relationship})
	}
	if l.character != "" {
		layers = append(layers, systemLayer{text: l.character, truncatable: true, prio: 1})
	}
	if l.lorebook != "" {
		layers = append(layers, systemLayer{text: "【背景知识】（相关时才用，自然带出，不要生硬复述）：\n" + l.lorebook, truncatable: true, prio: 2})
	}
	if l.relations != "" {
		layers = append(layers, systemLayer{text: l.relations, truncatable: true, prio: 3})
	}
	if l.current != "" {
		layers = append(layers, systemLayer{text: l.current})
	}
	if strings.TrimSpace(systemPrompt) != "" {
		layers = append(layers, systemLayer{text: systemPrompt, truncatable: true, prio: 0})
	}
	layers = enforceSystemBudget(layers)
	out := make([]string, 0, len(layers))
	for _, sl := range layers {
		out = append(out, sl.text)
	}
	return out
}
