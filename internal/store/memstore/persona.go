package memstore

import "github.com/openilink/openilink-hub/internal/store"

// Persona-related stores are not used by the app mock server. Methods exist to
// satisfy the store.Store interface and return errNotImplemented.

func (s *Store) SaveCharacter(c *store.Character) error           { return errNotImplemented }
func (s *Store) GetCharacter(id string) (*store.Character, error) { return nil, errNotImplemented }
func (s *Store) ListCharacters() ([]store.Character, error)       { return nil, errNotImplemented }
func (s *Store) DeleteCharacter(id string) error                  { return errNotImplemented }
func (s *Store) ListWorldEntries(characterID string) ([]store.WorldEntry, error) {
	return nil, errNotImplemented
}
func (s *Store) SaveWorldEntry(e *store.WorldEntry) error { return errNotImplemented }
func (s *Store) DeleteWorldEntry(id string) error         { return errNotImplemented }

func (s *Store) UpsertContact(c *store.Contact) error { return errNotImplemented }
func (s *Store) GetContact(botID, sender string) (*store.Contact, error) {
	return nil, errNotImplemented
}
func (s *Store) ListContacts(botID string) ([]store.Contact, error) { return nil, errNotImplemented }
func (s *Store) SetOwner(botID, sender string) error                { return errNotImplemented }
func (s *Store) DeleteContact(botID, sender string) error           { return errNotImplemented }
func (s *Store) UpsertRelation(r *store.ContactRelation) error      { return errNotImplemented }
func (s *Store) ListRelations(botID string) ([]store.ContactRelation, error) {
	return nil, errNotImplemented
}
func (s *Store) DeleteRelation(id string) error { return errNotImplemented }

func (s *Store) AddMemory(m *store.Memory) error { return errNotImplemented }
func (s *Store) ListMemories(botID, sender string) ([]store.Memory, error) {
	return nil, errNotImplemented
}
func (s *Store) ListMemoriesByBot(botID string) ([]store.Memory, error) {
	return nil, errNotImplemented
}
func (s *Store) UpdateMemory(m *store.Memory) error { return errNotImplemented }
func (s *Store) DeleteMemory(id string) error       { return errNotImplemented }
func (s *Store) CountMemories(botID, sender string) (int, error) {
	return 0, errNotImplemented
}

func (s *Store) AddScheduledMessage(m *store.ScheduledMessage) error { return errNotImplemented }
func (s *Store) ListDueScheduledMessages(now int64) ([]store.ScheduledMessage, error) {
	return nil, errNotImplemented
}
func (s *Store) ListPendingByScope(botID, sender, origin string) ([]store.ScheduledMessage, error) {
	return nil, errNotImplemented
}
func (s *Store) ClaimScheduledMessage(id string) (bool, error) { return false, errNotImplemented }
func (s *Store) MarkScheduledMessageSent(id string, sentAt int64) error {
	return errNotImplemented
}
func (s *Store) MarkScheduledMessageFailed(id, reason string) error {
	return errNotImplemented
}
func (s *Store) CancelScheduledMessage(id string) error { return errNotImplemented }
func (s *Store) CancelScheduledMessageWithReason(id, reason string) error {
	return errNotImplemented
}
func (s *Store) ListScheduledMessages(botID string, limit int) ([]store.ScheduledMessage, error) {
	return nil, errNotImplemented
}

func (s *Store) GetPersonaRules(botID string) (*store.PersonaRules, error) {
	return &store.PersonaRules{
		ID:        "default",
		BotID:     botID,
		Enabled:   true,
		RulesText: store.DefaultPersonaRulesText,
	}, nil
}
func (s *Store) SavePersonaRules(r *store.PersonaRules) error { return errNotImplemented }
