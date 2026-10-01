package store

import (
	"bytes"
	"testing"

	"charm.land/fantasy"
)

// A person's attached image is a user entry's fantasy.FilePart, its bytes
// inline as Fantasy's JSON writes them (plan 033 §3.5, P4): the store takes
// it, writes it, and reads it back exactly — the path, the type and every
// byte — so a resume, a fork or a replay sends the same image without the
// attachments directory.

// TestAUserEntryWithAnImageRoundTrips: the entry is written with the turn's
// answer and comes back, from the file, with its text part first and its
// file part after it, byte for byte; the context replays it to a later model
// (the vision strip is the harness's, not the store's). hasText — the check
// an answer must pass — reads an image entry by its text: a message with a
// file part and no words in it has none.
func TestAUserEntryWithAnImageRoundTrips(t *testing.T) {
	data := []byte("\x89PNG\r\n\x1a\nnot really the rest of a png, \x00\xff\x10 and some binary")
	file := fantasy.FilePart{Filename: "/home/u/.craze/attachments/0123456789abcdef.png", MediaType: "image/png", Data: data}
	s := newStore(t, testOptions(t))
	prompt := MessageEntry{Message: fantasy.NewUserMessage("what is [Image #1]?", file), Model: kimi}
	if err := s.AppendUser(prompt); err != nil {
		t.Fatalf("AppendUser: %v", err)
	}
	if err := s.AppendAssistant(answer("", "a chart", kimi)); err != nil {
		t.Fatalf("AppendAssistant: %v", err)
	}

	tr, err := Load(s.Path())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for name, ctx := range map[string][]fantasy.Message{"the same model": tr.Context(kimi), "a later model": tr.Context(minimax)} {
		if len(ctx) != 2 || ctx[0].Role != fantasy.MessageRoleUser || len(ctx[0].Content) != 2 {
			t.Fatalf("%s: the context = %v; want the prompt, text then file, and the answer", name, messageTexts(ctx))
		}
		text, ok := fantasy.AsMessagePart[fantasy.TextPart](ctx[0].Content[0])
		if !ok || text.Text != "what is [Image #1]?" {
			t.Fatalf("%s: the prompt's first part = %#v; want its text", name, ctx[0].Content[0])
		}
		got, ok := fantasy.AsMessagePart[fantasy.FilePart](ctx[0].Content[1])
		if !ok || got.Filename != file.Filename || got.MediaType != file.MediaType || !bytes.Equal(got.Data, data) {
			t.Fatalf("%s: the prompt's file part = %+v; want %+v back exactly", name, ctx[0].Content[1], file)
		}
	}

	if !hasText(prompt.Message) {
		t.Fatal("hasText: a prompt with its chip's text and an image reads as having no text")
	}
	imageOnly := fantasy.Message{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{file}}
	if hasText(imageOnly) {
		t.Fatal("hasText: a message holding a file part alone reads as having text")
	}
}
