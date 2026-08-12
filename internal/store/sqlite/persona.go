package sqlite

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/openilink/openilink-hub/internal/store"
)

func scanCharacter(row interface{ Scan(...any) error }) (*store.Character, error) {
	c := &store.Character{}
	err := row.Scan(&c.ID, &c.Name, &c.Spec, &c.CardJSON, &c.AvatarKey, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (db *DB) SaveCharacter(c *store.Character) error {
	now := db.now()
	if c.ID == "" {
		c.ID = uuid.New().String()
	}
	_, err := db.Exec(`
		INSERT INTO characters (id, name, spec, card_json, avatar_key, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			name = excluded.name, spec = excluded.spec, card_json = excluded.card_json,
			avatar_key = excluded.avatar_key, updated_at = excluded.updated_at`,
		c.ID, c.Name, c.Spec, c.CardJSON, c.AvatarKey, now, now)
	if err != nil {
		return fmt.Errorf("save character: %w", err)
	}
	c.CreatedAt = now
	c.UpdatedAt = now
	return nil
}

func (db *DB) GetCharacter(id string) (*store.Character, error) {
	row := db.QueryRow(`SELECT id, name, spec, card_json, avatar_key, created_at, updated_at FROM characters WHERE id = ?`, id)
	c, err := scanCharacter(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (db *DB) ListCharacters() ([]store.Character, error) {
	rows, err := db.Query(`SELECT id, name, spec, card_json, avatar_key, created_at, updated_at FROM characters ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Character
	for rows.Next() {
		c, err := scanCharacter(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func (db *DB) DeleteCharacter(id string) error {
	_, err := db.Exec(`DELETE FROM characters WHERE id = ?`, id)
	return err
}

func scanWorldEntry(row interface{ Scan(...any) error }) (*store.WorldEntry, error) {
	e := &store.WorldEntry{}
	var charID sql.NullString
	var enabled int
	err := row.Scan(&e.ID, &charID, &e.Keys, &e.Content, &enabled, &e.Priority, &e.CreatedAt)
	if err != nil {
		return nil, err
	}
	e.CharacterID = charID.String
	e.Enabled = enabled != 0
	return e, nil
}

// ListWorldEntries returns the character's entries plus global ones
// (character_id IS NULL). With an empty characterID only global entries are
// returned.
func (db *DB) ListWorldEntries(characterID string) ([]store.WorldEntry, error) {
	var rows *sql.Rows
	var err error
	if characterID == "" {
		rows, err = db.Query(`SELECT id, character_id, keys, content, enabled, priority, created_at FROM world_entries WHERE character_id IS NULL OR character_id = '' ORDER BY priority DESC, created_at`)
	} else {
		rows, err = db.Query(`SELECT id, character_id, keys, content, enabled, priority, created_at FROM world_entries WHERE character_id = ? OR character_id IS NULL OR character_id = '' ORDER BY priority DESC, created_at`, characterID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.WorldEntry
	for rows.Next() {
		e, err := scanWorldEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

func (db *DB) SaveWorldEntry(e *store.WorldEntry) error {
	now := db.now()
	if e.ID == "" {
		e.ID = uuid.New().String()
	}
	enabled := 0
	if e.Enabled {
		enabled = 1
	}
	// Global entries are stored as NULL (an empty string would violate the
	// characters(id) foreign key under PRAGMA foreign_keys=ON).
	var charID any
	if e.CharacterID != "" {
		charID = e.CharacterID
	}
	_, err := db.Exec(`
		INSERT INTO world_entries (id, character_id, keys, content, enabled, priority, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			character_id = excluded.character_id, keys = excluded.keys, content = excluded.content,
			enabled = excluded.enabled, priority = excluded.priority`,
		e.ID, charID, e.Keys, e.Content, enabled, e.Priority, now)
	if err != nil {
		return fmt.Errorf("save world entry: %w", err)
	}
	e.CreatedAt = now
	return nil
}

func (db *DB) DeleteWorldEntry(id string) error {
	_, err := db.Exec(`DELETE FROM world_entries WHERE id = ?`, id)
	return err
}

func scanContact(row interface{ Scan(...any) error }) (*store.Contact, error) {
	c := &store.Contact{}
	var owner int
	err := row.Scan(&c.ID, &c.BotID, &c.Sender, &c.Name, &c.Brief, &c.Profile, &owner, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	c.IsOwner = owner != 0
	return c, nil
}

func (db *DB) UpsertContact(c *store.Contact) error {
	now := db.now()
	if c.ID == "" {
		c.ID = uuid.New().String()
	}
	owner := 0
	if c.IsOwner {
		owner = 1
	}
	_, err := db.Exec(`
		INSERT INTO contacts (id, bot_id, sender, name, brief, profile, is_owner, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (bot_id, sender) DO UPDATE SET
			name = excluded.name, brief = excluded.brief, profile = excluded.profile,
			is_owner = excluded.is_owner, updated_at = excluded.updated_at`,
		c.ID, c.BotID, c.Sender, c.Name, c.Brief, c.Profile, owner, now, now)
	if err != nil {
		return fmt.Errorf("upsert contact: %w", err)
	}
	c.UpdatedAt = now
	return nil
}

func (db *DB) GetContact(botID, sender string) (*store.Contact, error) {
	row := db.QueryRow(`SELECT id, bot_id, sender, name, brief, profile, is_owner, created_at, updated_at FROM contacts WHERE bot_id = ? AND sender = ?`, botID, sender)
	c, err := scanContact(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (db *DB) ListContacts(botID string) ([]store.Contact, error) {
	rows, err := db.Query(`SELECT id, bot_id, sender, name, brief, profile, is_owner, created_at, updated_at FROM contacts WHERE bot_id = ? ORDER BY is_owner DESC, updated_at DESC`, botID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Contact
	for rows.Next() {
		c, err := scanContact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// SetOwner marks a sender as the bot's owner and clears any previous owner on
// the same bot (application-level guarantee of at most one owner).
func (db *DB) SetOwner(botID, sender string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE contacts SET is_owner = 0, updated_at = ? WHERE bot_id = ? AND is_owner = 1`, db.now(), botID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE contacts SET is_owner = 1, updated_at = ? WHERE bot_id = ? AND sender = ?`, db.now(), botID, sender); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) DeleteContact(botID, sender string) error {
	_, err := db.Exec(`DELETE FROM contacts WHERE bot_id = ? AND sender = ?`, botID, sender)
	return err
}

func scanRelation(row interface{ Scan(...any) error }) (*store.ContactRelation, error) {
	r := &store.ContactRelation{}
	err := row.Scan(&r.ID, &r.BotID, &r.FromSender, &r.ToSender, &r.Relation, &r.Note, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func (db *DB) UpsertRelation(r *store.ContactRelation) error {
	now := db.now()
	if r.ID == "" {
		r.ID = uuid.New().String()
	}
	_, err := db.Exec(`
		INSERT INTO contact_relations (id, bot_id, from_sender, to_sender, relation, note, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			from_sender = excluded.from_sender, to_sender = excluded.to_sender,
			relation = excluded.relation, note = excluded.note, updated_at = excluded.updated_at`,
		r.ID, r.BotID, r.FromSender, r.ToSender, r.Relation, r.Note, now, now)
	if err != nil {
		return fmt.Errorf("upsert relation: %w", err)
	}
	r.UpdatedAt = now
	return nil
}

func (db *DB) ListRelations(botID string) ([]store.ContactRelation, error) {
	rows, err := db.Query(`SELECT id, bot_id, from_sender, to_sender, relation, note, created_at, updated_at FROM contact_relations WHERE bot_id = ? ORDER BY created_at`, botID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.ContactRelation
	for rows.Next() {
		r, err := scanRelation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func (db *DB) DeleteRelation(id string) error {
	_, err := db.Exec(`DELETE FROM contact_relations WHERE id = ?`, id)
	return err
}

func scanMemory(row interface{ Scan(...any) error }) (*store.Memory, error) {
	m := &store.Memory{}
	err := row.Scan(&m.ID, &m.BotID, &m.Sender, &m.Category, &m.Content, &m.Importance, &m.ExpiresAt, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return m, nil
}

func (db *DB) AddMemory(m *store.Memory) error {
	now := db.now()
	if m.ID == "" {
		m.ID = uuid.New().String()
	}
	createdAt := m.CreatedAt
	if createdAt <= 0 {
		createdAt = now
	}
	_, err := db.Exec(`
		INSERT INTO memories (id, bot_id, sender, category, content, importance, expires_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.BotID, m.Sender, m.Category, m.Content, m.Importance, m.ExpiresAt, createdAt, now)
	if err != nil {
		return fmt.Errorf("add memory: %w", err)
	}
	m.CreatedAt = createdAt
	m.UpdatedAt = now
	return nil
}

func (db *DB) ListMemories(botID, sender string) ([]store.Memory, error) {
	// H1: expires_at auto-forget — expired memories are excluded at the SQL
	// layer (nil expiry means never expires).
	rows, err := db.Query(`SELECT id, bot_id, sender, category, content, importance, expires_at, created_at, updated_at FROM memories WHERE bot_id = ? AND sender = ? AND (expires_at IS NULL OR expires_at > ?) ORDER BY importance DESC, created_at DESC`, botID, sender, db.now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func (db *DB) ListMemoriesByBot(botID string) ([]store.Memory, error) {
	rows, err := db.Query(`SELECT id, bot_id, sender, category, content, importance, expires_at, created_at, updated_at FROM memories WHERE bot_id = ? ORDER BY importance DESC, created_at DESC`, botID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func (db *DB) UpdateMemory(m *store.Memory) error {
	_, err := db.Exec(`
		UPDATE memories SET category = ?, content = ?, importance = ?, expires_at = ?, updated_at = ? WHERE id = ?`,
		m.Category, m.Content, m.Importance, m.ExpiresAt, db.now(), m.ID)
	return err
}

func (db *DB) DeleteMemory(id string) error {
	_, err := db.Exec(`DELETE FROM memories WHERE id = ?`, id)
	return err
}

func (db *DB) CountMemories(botID, sender string) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM memories WHERE bot_id = ? AND sender = ?`, botID, sender).Scan(&n)
	return n, err
}

func scanScheduledMessage(row interface{ Scan(...any) error }) (*store.ScheduledMessage, error) {
	m := &store.ScheduledMessage{}
	err := row.Scan(&m.ID, &m.BotID, &m.Sender, &m.FireAt, &m.Tolerance, &m.Prompt, &m.Origin, &m.Status, &m.FailReason, &m.CreatedAt, &m.SentAt, &m.RescheduleCount)
	if err != nil {
		return nil, err
	}
	return m, nil
}

const schedCols = `id, bot_id, sender, fire_at, tolerance, prompt, origin, status, fail_reason, created_at, sent_at, reschedule_count`

func (db *DB) AddScheduledMessage(m *store.ScheduledMessage) error {
	now := db.now()
	if m.ID == "" {
		m.ID = uuid.New().String()
	}
	if m.Origin == "" {
		m.Origin = "promise"
	}
	if m.Status == "" {
		m.Status = "pending"
	}
	_, err := db.Exec(`
		INSERT INTO scheduled_messages (id, bot_id, sender, fire_at, tolerance, prompt, origin, status, fail_reason, created_at, sent_at, reschedule_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.BotID, m.Sender, m.FireAt, m.Tolerance, m.Prompt, m.Origin, m.Status, m.FailReason, now, m.SentAt, m.RescheduleCount)
	if err != nil {
		return fmt.Errorf("add scheduled message: %w", err)
	}
	m.CreatedAt = now
	return nil
}

func (db *DB) ListDueScheduledMessages(now int64) ([]store.ScheduledMessage, error) {
	rows, err := db.Query(`SELECT `+schedCols+` FROM scheduled_messages WHERE status = 'pending' AND fire_at <= ? ORDER BY fire_at`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.ScheduledMessage
	for rows.Next() {
		m, err := scanScheduledMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func (db *DB) ListPendingByScope(botID, sender, origin string) ([]store.ScheduledMessage, error) {
	rows, err := db.Query(`SELECT `+schedCols+` FROM scheduled_messages WHERE bot_id = ? AND sender = ? AND origin = ? AND status = 'pending' ORDER BY created_at`, botID, sender, origin)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.ScheduledMessage
	for rows.Next() {
		m, err := scanScheduledMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func (db *DB) ClaimScheduledMessage(id string) (bool, error) {
	res, err := db.Exec(`UPDATE scheduled_messages SET status = 'processing' WHERE id = ? AND status = 'pending'`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func (db *DB) MarkScheduledMessageSent(id string, sentAt int64) error {
	_, err := db.Exec(`UPDATE scheduled_messages SET status = 'sent', sent_at = ? WHERE id = ?`, sentAt, id)
	return err
}

func (db *DB) MarkScheduledMessageFailed(id, reason string) error {
	_, err := db.Exec(`UPDATE scheduled_messages SET status = 'failed', fail_reason = ? WHERE id = ?`, reason, id)
	return err
}

func (db *DB) CancelScheduledMessage(id string) error {
	_, err := db.Exec(`UPDATE scheduled_messages SET status = 'cancelled' WHERE id = ?`, id)
	return err
}

func (db *DB) CancelScheduledMessageWithReason(id, reason string) error {
	_, err := db.Exec(`UPDATE scheduled_messages SET status = 'cancelled', fail_reason = ? WHERE id = ?`, reason, id)
	return err
}

func (db *DB) ListScheduledMessages(botID string, limit int) ([]store.ScheduledMessage, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.Query(`SELECT `+schedCols+` FROM scheduled_messages WHERE bot_id = ? ORDER BY created_at DESC LIMIT ?`, botID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.ScheduledMessage
	for rows.Next() {
		m, err := scanScheduledMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func (db *DB) GetPersonaRules(botID string) (*store.PersonaRules, error) {
	// Level 1: bot-specific row.
	var r store.PersonaRules
	var enabled int
	err := db.QueryRow(`SELECT id, bot_id, enabled, rules_text, created_at, updated_at FROM persona_rules WHERE bot_id = ?`, botID).Scan(&r.ID, &r.BotID, &enabled, &r.RulesText, &r.CreatedAt, &r.UpdatedAt)
	if err == nil {
		r.Enabled = enabled != 0
		return &r, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	// Level 2: global row.
	err = db.QueryRow(`SELECT id, bot_id, enabled, rules_text, created_at, updated_at FROM persona_rules WHERE bot_id = '' OR bot_id IS NULL`).Scan(&r.ID, &r.BotID, &enabled, &r.RulesText, &r.CreatedAt, &r.UpdatedAt)
	if err == nil {
		r.Enabled = enabled != 0
		return &r, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	// Level 3: built-in defaults (not persisted).
	return &store.PersonaRules{
		ID:        "default",
		BotID:     botID,
		Enabled:   true,
		RulesText: store.DefaultPersonaRulesText,
	}, nil
}

func (db *DB) SavePersonaRules(r *store.PersonaRules) error {
	now := db.now()
	if r.ID == "" {
		r.ID = "default"
	}
	if r.BotID == "" {
		r.BotID = "" // global default
	}
	enabled := 0
	if r.Enabled {
		enabled = 1
	}
	_, err := db.Exec(`
		INSERT INTO persona_rules (id, bot_id, enabled, rules_text, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			bot_id = excluded.bot_id, enabled = excluded.enabled,
			rules_text = excluded.rules_text, updated_at = excluded.updated_at`,
		r.ID, r.BotID, enabled, r.RulesText, now, now)
	if err != nil {
		return fmt.Errorf("save persona rules: %w", err)
	}
	r.UpdatedAt = now
	return nil
}
