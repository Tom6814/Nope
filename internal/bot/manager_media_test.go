package bot

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/openilink/openilink-hub/internal/provider"
	"github.com/openilink/openilink-hub/internal/store"
	"github.com/openilink/openilink-hub/internal/store/sqlite"
)

func TestDownloadMedia_UpdatesOnlyTargetMessage(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "media.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	user, err := db.CreateUser("media_owner", "Media Owner")
	if err != nil {
		t.Fatal(err)
	}
	botRow, err := db.CreateBot(user.ID, "MediaBot", "test", "wx_bot", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}

	msg1ID := saveDownloadingMessage(t, db, botRow.ID, int64(101))
	msg2ID := saveDownloadingMessage(t, db, botRow.ID, int64(202))

	m := &Manager{
		store:   db,
		baseURL: "https://hub.example.com",
		dlSem:   make(chan struct{}, 1),
	}
	inst := NewInstance(botRow.ID, &fakeProvider{})
	msg := provider.InboundMessage{
		ExternalID: "202",
		Items: []provider.MessageItem{{
			Type: "image",
			Media: &provider.Media{
				EncryptQueryParam: "eqp-202",
				AESKey:            "aes-202",
			},
		}},
	}

	m.downloadMedia(inst, msg, msg2ID)

	got1, err := db.GetMessage(msg1ID)
	if err != nil {
		t.Fatal(err)
	}
	if got1.MediaStatus != "downloading" {
		t.Fatalf("message1 media_status = %q, want downloading", got1.MediaStatus)
	}

	got2, err := db.GetMessage(msg2ID)
	if err != nil {
		t.Fatal(err)
	}
	if got2.MediaStatus != "ready" {
		t.Fatalf("message2 media_status = %q, want ready", got2.MediaStatus)
	}
	if string(got2.MediaKeys) == "{}" || len(got2.MediaKeys) == 0 {
		t.Fatal("message2 media_keys should be populated")
	}
}

func saveDownloadingMessage(t *testing.T, db *sqlite.DB, botID string, messageID int64) int64 {
	t.Helper()
	res, err := db.SaveMessage(&store.Message{
		BotID:       botID,
		Direction:   "inbound",
		MessageID:   &messageID,
		FromUserID:  "wxid_a",
		ToUserID:    "wx_bot",
		ItemList:    json.RawMessage(`[{"type":"image","text":"img"}]`),
		MediaStatus: "downloading",
		MediaKeys:   json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("SaveMessage(%d): %v", messageID, err)
	}
	return res.ID
}
