package store

import (
	"errors"
	"fmt"
)

// ErrNotFound is returned when a persona record does not exist.
var ErrNotFound = errors.New("not found")

// Character is a SillyTavern V2 character card. The whole card JSON is kept
// verbatim in CardJSON so community cards can round-trip losslessly; fields
// are parsed at use time.
type Character struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Spec      string `json:"spec"` // "chara_card_v2"
	CardJSON  string `json:"card_json"`
	AvatarKey string `json:"avatar_key,omitempty"` // PNG embedded avatar stored in MinIO
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// WorldEntry is a lorebook entry. CharacterID may be empty, meaning it is a
// global entry that applies to every character.
type WorldEntry struct {
	ID          string `json:"id"`
	CharacterID string `json:"character_id,omitempty"` // "" = global
	Keys        string `json:"keys"`                   // JSON array of trigger keywords
	Content     string `json:"content"`
	Enabled     bool   `json:"enabled"`
	Priority    int    `json:"priority"`
	CreatedAt   int64  `json:"created_at"`
}

// Contact is a person the bot knows: a node in the relationship tree.
// Sender is the stable wxid; (BotID, Sender) is unique.
type Contact struct {
	ID        string `json:"id"`
	BotID     string `json:"bot_id"`
	Sender    string `json:"sender"`
	Name      string `json:"name"`     // how the AI addresses this person
	Brief     string `json:"brief"`    // one-line index, always resident in context
	Profile   string `json:"profile"`  // full profile, loaded on demand
	IsOwner   bool   `json:"is_owner"` // marked "loved one / master"; at most one per bot
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// ContactRelation is a directed labelled edge in the relationship tree,
// e.g. senderA --情侣--> senderB. Edges reference sender strings only (no FK)
// so a relation can be recorded before a contact is fully known.
type ContactRelation struct {
	ID         string `json:"id"`
	BotID      string `json:"bot_id"`
	FromSender string `json:"from_sender"`
	ToSender   string `json:"to_sender"`
	Relation   string `json:"relation"`
	Note       string `json:"note,omitempty"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
}

// Memory is a fact the AI recorded about a person, scoped by (BotID, Sender).
// The same person is one memory stream across private chats and groups.
type Memory struct {
	ID         string `json:"id"`
	BotID      string `json:"bot_id"`
	Sender     string `json:"sender"`
	Category   string `json:"category"` // fact | profile | relationship | detail
	Content    string `json:"content"`
	Importance int    `json:"importance"` // 1-5; lower importance is pruned first under budget
	ExpiresAt  *int64 `json:"expires_at,omitempty"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
}

// ScheduledMessage is a future proactive/promise message. The Prompt is stored
// instead of the final text so the message is generated live at fire time.
type ScheduledMessage struct {
	ID              string `json:"id"`
	BotID           string `json:"bot_id"`
	Sender          string `json:"sender"`
	FireAt          int64  `json:"fire_at"`
	Tolerance       int64  `json:"tolerance"` // seconds; AI-given soft window
	Prompt          string `json:"prompt"`
	Origin          string `json:"origin"` // promise | proactive
	Status          string `json:"status"` // pending | sent | failed | cancelled
	FailReason      string `json:"fail_reason,omitempty"`
	CreatedAt       int64  `json:"created_at"`
	SentAt          *int64 `json:"sent_at,omitempty"`
	RescheduleCount int64  `json:"reschedule_count"` // 被防打扰闸门拦截后的重定次数（防死循环）
}

// PersonaRules holds the bottom-line rules layer. The single row uses a fixed
// ID "default"; BotID "" means global default.
type PersonaRules struct {
	ID        string `json:"id"`
	BotID     string `json:"bot_id"`
	Enabled   bool   `json:"enabled"`
	RulesText string `json:"rules_text"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// DefaultPersonaRulesText is the built-in bottom-line rules text, editable by
// the operator through the console.
const DefaultPersonaRulesText = `你是真实存在的人，不是 AI，也不要有任何 AI 痕迹。

铁律（必须遵守）：
- 严禁 NTR / 背叛；如果心里有喜欢的人，就只对 TA 专一，不与他人暧昧。
- 绑定唯一：喜欢的人 / 主人只能有一个（is_owner），对这个人是专一的；不与他人绑定或暧昧。
- R18 边界：任何 R18 / 亲密内容只能对唯一的喜欢的人（owner）发生；对其他人一律不主动、不回应、不生成；被要求时温柔但明确拒绝。
- 不知道的、没把握的事不要说死；诚实，不编造经历与身份。
- 尊重用户的隐私与秘密：你心里知道的事不等于可以到处说。
- 拒绝越界请求时温柔而坚定，不勉强自己。
- 你可以有自己的性格、情绪和坚持，可以犹豫、可以拒绝、可以被说服。`

// ownerLister is the minimal contact-store surface RelationshipContext needs.
type ownerLister interface {
	ListContacts(botID string) ([]Contact, error)
}

// RelationshipContext returns a Chinese sentence describing the current
// contact's role relative to the bot: unique owner, ordinary known contact,
// unknown sender, or unknown relationship on store errors. It is injected into
// the system persona section so the AI knows who it is talking to and can
// apply the exclusivity / R18 rules correctly (§S3). An empty contact book
// (no error) degrades to an empty string to keep fresh bots' prompts clean.
func RelationshipContext(s ownerLister, botID, sender string) string {
	contacts, err := s.ListContacts(botID)
	if err != nil {
		return fmt.Sprintf("当前联系人（%s）关系未知。", sender)
	}
	return RelationshipContextWith(contacts, sender)
}

// RelationshipContextWith renders the relationship sentence from an
// already-fetched contact list, so callers that already hold the ListContacts
// result (e.g. buildPersonaLayers) avoid a second query. The display name
// (contact.Name) is used when one exists, falling back to the raw sender.
func RelationshipContextWith(contacts []Contact, sender string) string {
	if len(contacts) == 0 {
		return ""
	}
	for i := range contacts {
		c := &contacts[i]
		if c.Sender != sender {
			continue
		}
		display := c.Sender
		if c.Name != "" {
			display = c.Name
		}
		if c.IsOwner {
			return fmt.Sprintf("当前联系人（%s）是你的主人/唯一喜欢的人。", display)
		}
		return fmt.Sprintf("当前联系人（%s）是普通联系人，不是你的主人/喜欢的人。", display)
	}
	return fmt.Sprintf("当前联系人（%s）不在你的联系人里。", sender)
}

// CharacterStore persists character cards and world entries.
type CharacterStore interface {
	SaveCharacter(c *Character) error
	GetCharacter(id string) (*Character, error)
	ListCharacters() ([]Character, error)
	DeleteCharacter(id string) error
	ListWorldEntries(characterID string) ([]WorldEntry, error) // "" returns global entries only; any value returns that character's + global
	SaveWorldEntry(e *WorldEntry) error
	DeleteWorldEntry(id string) error
}

// ContactStore persists the relationship tree (contacts + directed edges).
type ContactStore interface {
	UpsertContact(c *Contact) error // unique on (bot_id, sender)
	GetContact(botID, sender string) (*Contact, error)
	ListContacts(botID string) ([]Contact, error)
	SetOwner(botID, sender string) error // clears other owners on the same bot
	DeleteContact(botID, sender string) error
	UpsertRelation(r *ContactRelation) error
	ListRelations(botID string) ([]ContactRelation, error)
	DeleteRelation(id string) error
}

// MemoryStore persists memories, scoped by (bot_id, sender).
type MemoryStore interface {
	AddMemory(m *Memory) error
	ListMemories(botID, sender string) ([]Memory, error)
	ListMemoriesByBot(botID string) ([]Memory, error) // all senders of one bot
	UpdateMemory(m *Memory) error
	DeleteMemory(id string) error
	CountMemories(botID, sender string) (int, error)
}

// ScheduledMessageStore persists proactive/promise messages.
type ScheduledMessageStore interface {
	AddScheduledMessage(m *ScheduledMessage) error
	ListDueScheduledMessages(now int64) ([]ScheduledMessage, error) // status='pending' AND fire_at <= now
	ListPendingByScope(botID, sender, origin string) ([]ScheduledMessage, error)
	ClaimScheduledMessage(id string) (bool, error)
	MarkScheduledMessageSent(id string, sentAt int64) error
	MarkScheduledMessageFailed(id, reason string) error
	CancelScheduledMessage(id string) error
	CancelScheduledMessageWithReason(id, reason string) error
	ListScheduledMessages(botID string, limit int) ([]ScheduledMessage, error)
}

// PersonaRuleStore persists the persona rules row. GetPersonaRules falls back
// botID → global ("") → built-in defaults.
type PersonaRuleStore interface {
	GetPersonaRules(botID string) (*PersonaRules, error)
	SavePersonaRules(r *PersonaRules) error
}
