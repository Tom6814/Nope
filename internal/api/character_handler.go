package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/openilink/openilink-hub/internal/ai"
	"github.com/openilink/openilink-hub/internal/auth"
	"github.com/openilink/openilink-hub/internal/character"
	"github.com/openilink/openilink-hub/internal/persona"
	"github.com/openilink/openilink-hub/internal/store"
)

// GET /api/characters — list character cards
func (s *Server) handleListCharacters(w http.ResponseWriter, r *http.Request) {
	chars, err := s.Store.ListCharacters()
	if err != nil {
		jsonError(w, "query failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, chars)
}

// GET /api/characters/{id} — get one character card (with parsed summary)
func (s *Server) handleGetCharacter(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ch, err := s.Store.GetCharacter(id)
	if err != nil {
		jsonError(w, "角色卡不存在", http.StatusNotFound)
		return
	}
	world, _ := s.Store.ListWorldEntries(ch.ID)
	resp := struct {
		*store.Character
		WorldEntries []store.WorldEntry `json:"world_entries"`
	}{ch, world}
	writeJSON(w, resp)
}

// POST /api/characters/import — import a SillyTavern V2 card (JSON body or
// multipart file; PNG-embedded cards are auto-detected). The character_book is
// split into world entries (design §4.2).
func (s *Server) handleImportCharacter(w http.ResponseWriter, r *http.Request) {
	var data []byte
	var err error

	switch {
	case strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data"):
		if err := r.ParseMultipartForm(20 << 20); err != nil {
			jsonError(w, "invalid multipart form", http.StatusBadRequest)
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			jsonError(w, "缺少 file 字段", http.StatusBadRequest)
			return
		}
		defer file.Close()
		data, err = io.ReadAll(io.LimitReader(file, 25<<20))
		if err != nil {
			jsonError(w, "读取文件失败", http.StatusBadRequest)
			return
		}
	default:
		data, err = io.ReadAll(io.LimitReader(r.Body, 25<<20))
		if err != nil {
			jsonError(w, "读取请求失败", http.StatusBadRequest)
			return
		}
	}

	card, err := character.Parse(data)
	if err != nil {
		jsonError(w, "不是有效的 SillyTavern V2 角色卡: "+err.Error(), http.StatusBadRequest)
		return
	}
	if card.Name == "" {
		jsonError(w, "角色卡缺少 name 字段", http.StatusBadRequest)
		return
	}

	cardJSON := string(data)
	if len(data) >= 8 && data[0] == 0x89 {
		// PNG-embedded: re-parse to extract the plain JSON for storage.
		if _, err := character.Parse(data); err != nil {
			jsonError(w, "PNG 内嵌角色卡解析失败", http.StatusBadRequest)
			return
		}
		// Keep cardJSON as the parsed raw JSON so storage stays portable.
		cardJSON = string(card.Raw)
	}

	avatarKey := ""
	if len(card.Avatar) > 0 && s.ObjectStore != nil {
		key := "avatars/character_" + strings.ReplaceAll(card.Name, " ", "_") + ".png"
		if _, err := s.ObjectStore.Put(r.Context(), key, "image/png", card.Avatar); err != nil {
			slog.Warn("character: avatar store failed", "err", err)
		} else {
			avatarKey = key
		}
	}

	ch := &store.Character{
		Name:      card.Name,
		Spec:      "chara_card_v2",
		CardJSON:  cardJSON,
		AvatarKey: avatarKey,
	}
	if err := s.Store.SaveCharacter(ch); err != nil {
		jsonError(w, "保存角色卡失败", http.StatusInternalServerError)
		return
	}

	// Split character_book into world entries (design §4.2).
	saveWorldEntriesFromCard(s.Store, ch.ID, card)

	writeJSON(w, map[string]any{"id": ch.ID, "name": ch.Name, "world_entries": len(card.WorldEntries)})
}

// saveWorldEntriesFromCard splits a card's character_book into WorldEntry rows
// for the given character (design §4.2). It is shared by the import and the
// S4 generate paths so both persist lorebook entries the same way (M12). Save
// failures are logged and skipped, keeping import/generation non-fatal.
func saveWorldEntriesFromCard(st store.CharacterStore, characterID string, card *character.Card) {
	for _, e := range card.WorldEntries {
		keys, _ := json.Marshal(e.Keys)
		we := &store.WorldEntry{
			CharacterID: characterID,
			Keys:        string(keys),
			Content:     e.Content,
			Enabled:     e.Enabled,
			Priority:    e.Priority,
		}
		if err := st.SaveWorldEntry(we); err != nil {
			slog.Warn("character: world entry save failed", "err", err)
		}
	}
}

// generateCharacterSystemPrompt instructs the model to expand a description
// into a complete SillyTavern V2 character card (design §S4). The output is
// parsed by character.Parse, so the fields must match the V2 data object.
const generateCharacterSystemPrompt = `你是角色卡创作助手。用户会给你一段角色描述，请把它优化扩展成一张完整的 SillyTavern V2 角色卡。
只输出一个 JSON 对象，不要输出任何解释文字，不要用 markdown 代码块包裹。格式：
{"spec":"chara_card_v2","spec_version":"2.0","data":{"name":"角色名","description":"角色背景与设定","personality":"性格特征","scenario":"初始场景","first_mes":"开场白（角色说的第一句话）","mes_example":"对话示例"}}
要求：
- name 和 description 必须非空；所有字段都是字符串，内容用中文；
- description 尽量丰富：身份、经历、说话风格、禁忌；
- personality 写性格与行为特点，不要流水账；
- first_mes 是角色出场的第一句话，像真人发微信一样自然，不要写成说明文；
- mes_example 用 <START> 分隔的对话示例展示角色说话风格。`

// POST /api/persona/generate-character — generate a SillyTavern V2 character
// card from a free-text description via the configured AI, then save it like
// an imported card (design §S4).
func (s *Server) handleGenerateCharacter(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}
	req.Description = strings.TrimSpace(req.Description)
	if req.Description == "" {
		jsonError(w, "description required", http.StatusBadRequest)
		return
	}

	cfg := persona.ResolveAIConfig(s.Store)
	if cfg.APIKey == "" {
		jsonError(w, "请先配置 AI", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	messages := []ai.Message{
		{Role: "system", Content: generateCharacterSystemPrompt},
		{Role: "user", Content: req.Description},
	}
	result, err := ai.CompleteMessages(ctx, cfg, messages, nil)
	if err != nil {
		jsonError(w, "AI 调用失败: "+err.Error(), http.StatusInternalServerError)
		return
	}

	cardJSON := stripCodeFences(result.Content)
	card, err := character.Parse([]byte(cardJSON))
	if err != nil {
		jsonError(w, "AI 生成的不是有效的 SillyTavern V2 角色卡: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}
	if card.Description == "" {
		jsonError(w, "AI 生成的角色卡缺少 description 字段", http.StatusUnprocessableEntity)
		return
	}

	ch := &store.Character{
		Name:     card.Name,
		Spec:     "chara_card_v2",
		CardJSON: cardJSON,
	}
	if err := s.Store.SaveCharacter(ch); err != nil {
		jsonError(w, "保存角色卡失败", http.StatusInternalServerError)
		return
	}
	// 与导入路径一致，拆分 character_book 为世界书条目 (M12)。
	saveWorldEntriesFromCard(s.Store, ch.ID, card)
	writeJSON(w, ch)
}

// stripCodeFences removes a markdown code fence (```json / ``` ... ```) from
// an LLM reply if present — even when the model wraps the JSON with chatter
// around the fence — then trims whitespace.
func stripCodeFences(s string) string {
	s = strings.TrimSpace(s)
	if start := strings.Index(s, "```"); start >= 0 {
		rest := s[start+3:]
		if i := strings.IndexByte(rest, '\n'); i >= 0 {
			rest = rest[i+1:]
		}
		if end := strings.LastIndex(rest, "```"); end >= 0 {
			rest = rest[:end]
		}
		s = rest
	}
	return strings.TrimSpace(s)
}

// DELETE /api/characters/{id} — delete a character card
func (s *Server) handleDeleteCharacter(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Store.DeleteCharacter(id); err != nil {
		jsonError(w, "删除失败", http.StatusInternalServerError)
		return
	}
	jsonOK(w)
}

// GET /api/characters/{id}/world-entries — list a character's world entries
func (s *Server) handleListCharacterWorld(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	entries, err := s.Store.ListWorldEntries(id)
	if err != nil {
		jsonError(w, "query failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, entries)
}

// POST /api/characters/world-entries — save a world entry (upsert)
func (s *Server) handleSaveWorldEntry(w http.ResponseWriter, r *http.Request) {
	var e store.WorldEntry
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}
	if e.Content == "" {
		jsonError(w, "content required", http.StatusBadRequest)
		return
	}
	if err := s.Store.SaveWorldEntry(&e); err != nil {
		jsonError(w, "save failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, e)
}

// DELETE /api/characters/world-entries/{id} — delete a world entry
func (s *Server) handleDeleteWorldEntry(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Store.DeleteWorldEntry(id); err != nil {
		jsonError(w, "删除失败", http.StatusInternalServerError)
		return
	}
	jsonOK(w)
}

// GET /api/persona/rules — get the persona rules (three-level fallback)
func (s *Server) handleGetPersonaRules(w http.ResponseWriter, r *http.Request) {
	rules, err := s.Store.GetPersonaRules("")
	if err != nil {
		jsonError(w, "query failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, rules)
}

// PUT /api/persona/rules — save the persona rules
func (s *Server) handleSavePersonaRules(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled   *bool  `json:"enabled"`
		RulesText string `json:"rules_text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}
	rules, _ := s.Store.GetPersonaRules("")
	rules.RulesText = req.RulesText
	if req.Enabled != nil {
		rules.Enabled = *req.Enabled
	}
	if err := s.Store.SavePersonaRules(rules); err != nil {
		jsonError(w, "save failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, rules)
}

// GET /api/bots/{id}/scheduled-messages — list scheduled messages for a bot
func (s *Server) handleListScheduledMessages(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	userID := auth.UserIDFromContext(r.Context())
	bot, err := s.Store.GetBot(id)
	if err != nil || bot.UserID != userID {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	msgs, err := s.Store.ListScheduledMessages(id, 100)
	if err != nil {
		jsonError(w, "query failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, msgs)
}

// DELETE /api/bots/{id}/scheduled-messages/{msgID} — cancel a pending scheduled message
func (s *Server) handleCancelScheduledMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	msgID := r.PathValue("msgID")
	userID := auth.UserIDFromContext(r.Context())
	bot, err := s.Store.GetBot(id)
	if err != nil || bot.UserID != userID {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	all, err := s.Store.ListScheduledMessages(id, 500)
	if err != nil {
		jsonError(w, "failed to load scheduled messages", http.StatusInternalServerError)
		return
	}
	var found *store.ScheduledMessage
	for i := range all {
		if all[i].ID == msgID {
			found = &all[i]
			break
		}
	}
	if found == nil || found.BotID != id {
		jsonError(w, "not found or not owned", http.StatusNotFound)
		return
	}
	if found.Status != "pending" {
		jsonError(w, "只能取消待发送的消息", http.StatusConflict)
		return
	}
	if err := s.Store.CancelScheduledMessageWithReason(msgID, "控制台取消"); err != nil {
		jsonError(w, "cancel failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// GET /api/bots/{id}/scheduled-messages/stats?days=N — counts over recent N days (default 7, max 30)
func (s *Server) handleScheduledMessageStats(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	userID := auth.UserIDFromContext(r.Context())
	bot, err := s.Store.GetBot(id)
	if err != nil || bot.UserID != userID {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	days := 7
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 30 {
			days = n
		}
	}
	since := time.Now().Add(-time.Duration(days) * 24 * time.Hour).Unix()
	all, err := s.Store.ListScheduledMessages(id, 500)
	if err != nil {
		jsonError(w, "failed to load scheduled messages", http.StatusInternalServerError)
		return
	}
	byStatus := map[string]int{}
	byOrigin := map[string]int{}
	var recent []store.ScheduledMessage
	total := 0
	for _, m := range all {
		inWindow := m.CreatedAt >= since
		if m.SentAt != nil && *m.SentAt >= since {
			inWindow = true
		}
		if !inWindow {
			continue
		}
		total++
		byStatus[m.Status]++
		byOrigin[m.Origin]++
		if len(recent) < 10 {
			recent = append(recent, m)
		}
	}
	writeJSON(w, map[string]any{
		"total":     total,
		"by_status": byStatus,
		"by_origin": byOrigin,
		"recent":    recent,
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
