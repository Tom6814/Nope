-- +goose Up
CREATE TABLE IF NOT EXISTS characters (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    spec        TEXT NOT NULL DEFAULT 'chara_card_v2',
    card_json   TEXT NOT NULL,
    avatar_key  TEXT,
    created_at  INTEGER NOT NULL DEFAULT (unixepoch()),
    updated_at  INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE TABLE IF NOT EXISTS world_entries (
    id           TEXT PRIMARY KEY,
    character_id TEXT REFERENCES characters(id) ON DELETE CASCADE,
    keys         TEXT NOT NULL,
    content      TEXT NOT NULL,
    enabled      INTEGER NOT NULL DEFAULT 1,
    priority     INTEGER NOT NULL DEFAULT 0,
    created_at   INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE INDEX IF NOT EXISTS idx_world_entries_char ON world_entries(character_id);

CREATE TABLE IF NOT EXISTS contacts (
    id         TEXT PRIMARY KEY,
    bot_id     TEXT NOT NULL REFERENCES bots(id) ON DELETE CASCADE,
    sender     TEXT NOT NULL,
    name       TEXT NOT NULL DEFAULT '',
    brief      TEXT NOT NULL DEFAULT '',
    profile    TEXT NOT NULL DEFAULT '',
    is_owner   INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL DEFAULT (unixepoch()),
    updated_at INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_contacts_scope ON contacts(bot_id, sender);

CREATE TABLE IF NOT EXISTS contact_relations (
    id          TEXT PRIMARY KEY,
    bot_id      TEXT NOT NULL REFERENCES bots(id) ON DELETE CASCADE,
    from_sender TEXT NOT NULL,
    to_sender   TEXT NOT NULL,
    relation    TEXT NOT NULL,
    note        TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL DEFAULT (unixepoch()),
    updated_at  INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE INDEX IF NOT EXISTS idx_relations_bot ON contact_relations(bot_id);

CREATE TABLE IF NOT EXISTS memories (
    id          TEXT PRIMARY KEY,
    bot_id      TEXT NOT NULL REFERENCES bots(id) ON DELETE CASCADE,
    sender      TEXT NOT NULL,
    category    TEXT NOT NULL DEFAULT 'fact',
    content     TEXT NOT NULL,
    importance  INTEGER NOT NULL DEFAULT 1,
    expires_at  INTEGER,
    created_at  INTEGER NOT NULL DEFAULT (unixepoch()),
    updated_at  INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE INDEX IF NOT EXISTS idx_memories_scope ON memories(bot_id, sender);

CREATE TABLE IF NOT EXISTS scheduled_messages (
    id          TEXT PRIMARY KEY,
    bot_id      TEXT NOT NULL REFERENCES bots(id) ON DELETE CASCADE,
    sender      TEXT NOT NULL,
    fire_at     INTEGER NOT NULL,
    tolerance   INTEGER NOT NULL DEFAULT 600,
    prompt      TEXT NOT NULL,
    origin      TEXT NOT NULL DEFAULT 'promise',
    status      TEXT NOT NULL DEFAULT 'pending',
    fail_reason TEXT,
    created_at  INTEGER NOT NULL DEFAULT (unixepoch()),
    sent_at     INTEGER
);

CREATE INDEX IF NOT EXISTS idx_sched_due ON scheduled_messages(status, fire_at);

CREATE TABLE IF NOT EXISTS persona_rules (
    id         TEXT PRIMARY KEY,
    bot_id     TEXT NOT NULL DEFAULT '',
    enabled    INTEGER NOT NULL DEFAULT 1,
    rules_text TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL DEFAULT (unixepoch()),
    updated_at INTEGER NOT NULL DEFAULT (unixepoch())
);

-- +goose Down
DROP TABLE IF EXISTS persona_rules;
DROP TABLE IF EXISTS scheduled_messages;
DROP TABLE IF EXISTS memories;
DROP TABLE IF EXISTS contact_relations;
DROP TABLE IF EXISTS contacts;
DROP TABLE IF EXISTS world_entries;
DROP TABLE IF EXISTS characters;
