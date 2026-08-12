package store

import (
	"errors"
	"strings"
	"testing"
)

func TestDefaultPersonaRulesText_ContainsS3Clauses(t *testing.T) {
	for _, want := range []string{
		"绑定唯一",
		"只能有一个",
		"is_owner",
		"R18",
		"不主动、不回应、不生成",
	} {
		if !strings.Contains(DefaultPersonaRulesText, want) {
			t.Errorf("DefaultPersonaRulesText must contain %q", want)
		}
	}
	// 原有条款保留。
	for _, want := range []string{"NTR", "诚实", "隐私"} {
		if !strings.Contains(DefaultPersonaRulesText, want) {
			t.Errorf("DefaultPersonaRulesText must keep original clause %q", want)
		}
	}
}

// fakeContactLister is a fake of the narrow surface RelationshipContext needs.
type fakeContactLister struct {
	contacts []Contact
	err      error
}

func (f fakeContactLister) ListContacts(string) ([]Contact, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.contacts, nil
}

func TestRelationshipContext(t *testing.T) {
	owner := Contact{BotID: "b1", Sender: "wxid_owner", Name: "主人", IsOwner: true}
	friend := Contact{BotID: "b1", Sender: "wxid_friend", Name: "朋友"}

	cases := []struct {
		name   string
		store  fakeContactLister
		sender string
		want   string
	}{
		{
			name:   "owner uses display name",
			store:  fakeContactLister{contacts: []Contact{owner, friend}},
			sender: "wxid_owner",
			want:   "当前联系人（主人）是你的主人/唯一喜欢的人。",
		},
		{
			name:   "non-owner known contact uses display name",
			store:  fakeContactLister{contacts: []Contact{owner, friend}},
			sender: "wxid_friend",
			want:   "当前联系人（朋友）是普通联系人，不是你的主人/喜欢的人。",
		},
		{
			name:   "unknown sender keeps raw wxid",
			store:  fakeContactLister{contacts: []Contact{owner}},
			sender: "wxid_stranger",
			want:   "当前联系人（wxid_stranger）不在你的联系人里。",
		},
		{
			name:   "store error",
			store:  fakeContactLister{err: errors.New("db down")},
			sender: "wxid_owner",
			want:   "当前联系人（wxid_owner）关系未知。",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RelationshipContext(tc.store, "b1", tc.sender)
			if got != tc.want {
				t.Errorf("RelationshipContext() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRelationshipContextWith(t *testing.T) {
	owner := Contact{BotID: "b1", Sender: "wxid_owner", Name: "主人", IsOwner: true}
	friend := Contact{BotID: "b1", Sender: "wxid_friend", Name: "朋友"}

	cases := []struct {
		name     string
		contacts []Contact
		sender   string
		want     string
	}{
		{
			name:     "owner uses display name",
			contacts: []Contact{owner, friend},
			sender:   "wxid_owner",
			want:     "当前联系人（主人）是你的主人/唯一喜欢的人。",
		},
		{
			name:     "known contact without name falls back to wxid",
			contacts: []Contact{owner, {BotID: "b1", Sender: "wxid_nn"}},
			sender:   "wxid_nn",
			want:     "当前联系人（wxid_nn）是普通联系人，不是你的主人/喜欢的人。",
		},
		{
			name:     "unknown sender",
			contacts: []Contact{owner},
			sender:   "wxid_stranger",
			want:     "当前联系人（wxid_stranger）不在你的联系人里。",
		},
		{
			name:     "empty contact book degrades to empty string",
			contacts: nil,
			sender:   "wxid_a",
			want:     "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RelationshipContextWith(tc.contacts, tc.sender)
			if got != tc.want {
				t.Errorf("RelationshipContextWith() = %q, want %q", got, tc.want)
			}
		})
	}
}
