package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/openilink/openilink-hub/internal/auth"
	"github.com/openilink/openilink-hub/internal/store"
)

// memoryGroup is one sender's memories inside the grouped list response.
type memoryGroup struct {
	Sender   string         `json:"sender"`
	Memories []store.Memory `json:"memories"`
}

// nullableInt64 distinguishes an explicit JSON null (clear the value) from a
// missing field (keep the current value) — json.Unmarshal alone cannot.
type nullableInt64 struct {
	set   bool
	value *int64
}

func (n *nullableInt64) UnmarshalJSON(b []byte) error {
	n.set = true
	if string(b) == "null" {
		n.value = nil
		return nil
	}
	var v int64
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	n.value = &v
	return nil
}

// requireBotOwner resolves the {id} path value and verifies the requesting
// user owns the bot (same pattern as the S1 handlers). It writes a 404 and
// returns nil when the bot is missing or not owned.
func (s *Server) requireBotOwner(w http.ResponseWriter, r *http.Request) *store.Bot {
	id := r.PathValue("id")
	userID := auth.UserIDFromContext(r.Context())
	bot, err := s.Store.GetBot(id)
	if err != nil || bot.UserID != userID {
		jsonError(w, "not found", http.StatusNotFound)
		return nil
	}
	return bot
}

// GET /api/bots/{id}/memories?sender=wxid_x — list memories for a bot.
// Without ?sender the memories are grouped by sender ({groups: [...]}); with
// ?sender only that sender's group is returned (empty group included).
func (s *Server) handleListMemories(w http.ResponseWriter, r *http.Request) {
	if s.requireBotOwner(w, r) == nil {
		return
	}
	id := r.PathValue("id")

	if sender := r.URL.Query().Get("sender"); sender != "" {
		mems, err := s.Store.ListMemories(id, sender)
		if err != nil {
			jsonError(w, "query failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{
			"groups": []memoryGroup{{Sender: sender, Memories: mems}},
		})
		return
	}

	all, err := s.Store.ListMemoriesByBot(id)
	if err != nil {
		jsonError(w, "query failed", http.StatusInternalServerError)
		return
	}
	groups := []memoryGroup{}
	index := map[string]int{} // sender → position in groups
	for _, m := range all {
		if idx, ok := index[m.Sender]; ok {
			groups[idx].Memories = append(groups[idx].Memories, m)
			continue
		}
		index[m.Sender] = len(groups)
		groups = append(groups, memoryGroup{Sender: m.Sender, Memories: []store.Memory{m}})
	}
	writeJSON(w, map[string]any{"groups": groups})
}

// PUT /api/bots/{id}/memories/{memID} — update a memory. Body fields are
// optional: {content, importance, expires_at} (expires_at: null clears it).
func (s *Server) handleUpdateMemory(w http.ResponseWriter, r *http.Request) {
	bot := s.requireBotOwner(w, r)
	if bot == nil {
		return
	}
	id := r.PathValue("id")
	memID := r.PathValue("memID")

	all, err := s.Store.ListMemoriesByBot(id)
	if err != nil {
		jsonError(w, "failed to load memories", http.StatusInternalServerError)
		return
	}
	var found *store.Memory
	for i := range all {
		if all[i].ID == memID {
			found = &all[i]
			break
		}
	}
	if found == nil {
		jsonError(w, "not found or not owned", http.StatusNotFound)
		return
	}

	var req struct {
		Content    *string        `json:"content"`
		Importance *int           `json:"importance"`
		ExpiresAt  *nullableInt64 `json:"expires_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.Content != nil {
		trimmed := strings.TrimSpace(*req.Content)
		if trimmed == "" {
			jsonError(w, "content 不能为空", http.StatusBadRequest)
			return
		}
		found.Content = trimmed
	}
	if req.Importance != nil {
		found.Importance = *req.Importance
	}
	if req.ExpiresAt != nil && req.ExpiresAt.set {
		found.ExpiresAt = req.ExpiresAt.value
	}

	if err := s.Store.UpdateMemory(found); err != nil {
		jsonError(w, "update failed", http.StatusInternalServerError)
		return
	}
	// Re-read to return the persisted row (fresh updated_at).
	after, err := s.Store.ListMemories(id, found.Sender)
	if err != nil {
		jsonError(w, "update failed", http.StatusInternalServerError)
		return
	}
	for i := range after {
		if after[i].ID == memID {
			writeJSON(w, after[i])
			return
		}
	}
	writeJSON(w, *found)
}

// DELETE /api/bots/{id}/memories/{memID} — delete a memory.
func (s *Server) handleDeleteMemory(w http.ResponseWriter, r *http.Request) {
	bot := s.requireBotOwner(w, r)
	if bot == nil {
		return
	}
	id := r.PathValue("id")
	memID := r.PathValue("memID")

	all, err := s.Store.ListMemoriesByBot(id)
	if err != nil {
		jsonError(w, "failed to load memories", http.StatusInternalServerError)
		return
	}
	found := false
	for _, m := range all {
		if m.ID == memID {
			found = true
			break
		}
	}
	if !found {
		jsonError(w, "not found or not owned", http.StatusNotFound)
		return
	}
	if err := s.Store.DeleteMemory(memID); err != nil {
		jsonError(w, "delete failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
