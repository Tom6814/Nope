-- +goose Up
ALTER TABLE scheduled_messages ADD COLUMN reschedule_count INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE scheduled_messages DROP COLUMN reschedule_count;
