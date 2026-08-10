package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mymmrac/telego"
)

type responsePayload struct {
	Text          string                 `json:"text"`
	Entities      []telego.MessageEntity `json:"entities,omitempty"`
	Operation     string                 `json:"operation"`
	FallbackText  string                 `json:"fallback_text,omitempty"`
	FallbackItems []telego.MessageEntity `json:"fallback_entities,omitempty"`
}

func encodeResponsePayload(payload responsePayload) (string, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode response payload: %w", err)
	}
	return string(encoded), nil
}

func decodeResponsePayload(raw string) responsePayload {
	var payload responsePayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return responsePayload{Text: raw, Operation: operationText}
	}
	if payload.Operation == "" {
		payload.Operation = operationText
	}
	return payload
}

func responseEntities(text, senderName string, senderID int64) []telego.MessageEntity {
	if senderID <= 0 || senderName == "" {
		return nil
	}
	before, _, found := strings.Cut(text, senderName)
	if !found {
		return nil
	}
	return []telego.MessageEntity{{
		Type:   telego.EntityTypeTextMention,
		Offset: utf16Units(before),
		Length: utf16Units(senderName),
		User:   &telego.User{ID: senderID, FirstName: strings.TrimPrefix(senderName, "@")},
	}}
}

func utf16Units(value string) int {
	units := 0
	for _, r := range value {
		if r > 0xffff {
			units += 2
			continue
		}
		units++
	}
	return units
}
