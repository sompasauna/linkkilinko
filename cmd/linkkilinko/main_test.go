package main

import (
	"testing"

	"github.com/mymmrac/telego"
)

func TestResponseEntitiesUsesUTF16OffsetsAndUserID(t *testing.T) {
	text := "Käyttäjä 🙂 lähetti linkin"
	entities := responseEntities(text, "🙂", 42)
	if len(entities) != 1 {
		t.Fatalf("entities = %#v, want one text mention", entities)
	}
	entity := entities[0]
	if entity.Type != telego.EntityTypeTextMention || entity.Offset != 9 || entity.Length != 2 {
		t.Fatalf("entity = %#v, want UTF-16 mention span", entity)
	}
	if entity.User == nil || entity.User.ID != 42 {
		t.Fatalf("entity user = %#v, want user 42", entity.User)
	}
}

func TestResponsePayloadRoundTrip(t *testing.T) {
	original := responsePayload{
		Text:      "notice",
		Operation: operationText,
		Entities:  responseEntities("notice", "notice", 9),
	}
	encoded, err := encodeResponsePayload(original)
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodeResponsePayload(encoded)
	if decoded.Text != original.Text || decoded.Operation != original.Operation || len(decoded.Entities) != 1 || decoded.Entities[0].User.ID != 9 {
		t.Fatalf("decoded = %#v, want %#v", decoded, original)
	}
}

func TestOwnerMatchesOnlyPersistedOwner(t *testing.T) {
	tests := []struct {
		name  string
		found bool
		owner int64
		actor int64
		want  bool
	}{
		{name: "matching owner", found: true, owner: 42, actor: 42, want: true},
		{name: "different user", found: true, owner: 42, actor: 99},
		{name: "no owner", found: false, owner: 42, actor: 42},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ownerMatches(test.found, test.owner, test.actor); got != test.want {
				t.Fatalf("ownerMatches() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestGroupChatRecognition(t *testing.T) {
	if !isGroupChat(telego.ChatTypeGroup) || !isGroupChat(telego.ChatTypeSupergroup) {
		t.Fatal("group and supergroup chats must be recognized")
	}
	if isGroupChat(telego.ChatTypePrivate) {
		t.Fatal("private chats must not be recognized as groups")
	}
}
