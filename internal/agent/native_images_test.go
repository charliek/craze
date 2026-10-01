package agent

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/store"
)

// A native prompt's images (plan 033 §3.5, C5): the envelope comes off at the
// session, the host reads the images it names, and the turn gets them as file
// parts after its text — what the model is then sent of them is its vision's
// (the harness's strip) — while one the host would not read goes as its path
// text. A restored transcript shows the prompt's chip.

// withVision saves f's table again with alias marked as accepting images
// (vision = true), before the session that is to read it starts.
func (f *nativeFixture) withVision(alias string) {
	f.t.Helper()
	table := nativeTestTable("http://127.0.0.1:9/v1")
	m := table.Models[alias]
	m.Vision = true
	table.Models[alias] = m
	if err := modeltable.Save(f.dir, table); err != nil {
		f.t.Fatalf("saving the test model table: %v", err)
	}
}

// lastMessage is the last message a request carried: a turn's first
// request's is its prompt.
func lastMessage(t *testing.T, call fantasy.Call) fantasy.Message {
	t.Helper()
	if len(call.Prompt) == 0 {
		t.Fatal("the request carried no messages")
	}
	return call.Prompt[len(call.Prompt)-1]
}

// noEnvelope fails the test if any text the model was sent holds the
// attachment envelope's tag.
func noEnvelope(t *testing.T, call fantasy.Call) {
	t.Helper()
	for _, msg := range call.Prompt {
		for _, p := range msg.Content {
			if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](p); ok && strings.Contains(tp.Text, "craze_attachments") {
				t.Fatalf("the envelope reached the model: %q", tp.Text)
			}
		}
	}
}

// storedUser is the fixture's one transcript's first user entry: the message
// the first turn opened with, as the file holds it.
func storedUser(t *testing.T, f *nativeFixture) fantasy.Message {
	t.Helper()
	paths := f.transcripts()
	if len(paths) != 1 {
		t.Fatalf("%d transcripts, want 1", len(paths))
	}
	tr, err := store.Load(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range tr.Entries {
		if e.Type == store.TypeMessage && e.Message.Role == fantasy.MessageRoleUser {
			return e.Message
		}
	}
	t.Fatal("the transcript holds no user entry")
	return fantasy.Message{}
}

// TestNativePromptSendsItsImages: a chip's image reaches a model that accepts
// images as a file part after the prompt's words — the path, the type and
// the bytes the host read — and a model that does not as a placeholder naming
// the model and the file, while the transcript keeps the image either way.
// An image the host will not read is its path text, with the reason, and no
// file part. The envelope never reaches the model, and the session is titled
// from the words.
func TestNativePromptSendsItsImages(t *testing.T) {
	t.Run("a model that accepts images", func(t *testing.T) {
		f := newNativeFixture(t)
		f.withVision("test/a")
		ref, data := savedImage(t, 1, 40, 30)
		s := f.started(Options{})
		m := f.models["test/a"]
		m.push(answer("a grey square"))
		if _, err := s.Prompt(context.Background(), AttachmentBlock([]AttachmentRef{ref})+"look at [Image #1]"); err != nil {
			t.Fatal(err)
		}
		calls := m.requests()
		if len(calls) != 1 {
			t.Fatalf("%d requests", len(calls))
		}
		want := fantasy.NewUserMessage("look at [Image #1]", fantasy.FilePart{Filename: ref.Path, MediaType: "image/png", Data: data})
		if got := lastMessage(t, calls[0]); !reflect.DeepEqual(got, want) {
			t.Fatalf("the model was sent %#v; want the words, then the image as a file part", got)
		}
		noEnvelope(t, calls[0])
		if !reflect.DeepEqual(storedUser(t, f), want) {
			t.Fatal("the transcript does not hold the prompt as it was sent")
		}
		if title := s.Snapshot().Title; title != "look at [Image #1]" {
			t.Fatalf("title %q", title)
		}
	})

	t.Run("a model that does not", func(t *testing.T) {
		f := newNativeFixture(t)
		ref, data := savedImage(t, 1, 40, 30)
		s := f.started(Options{})
		m := f.models["test/a"]
		m.push(answer("I cannot see it"))
		if _, err := s.Prompt(context.Background(), AttachmentBlock([]AttachmentRef{ref})+"look at [Image #1]"); err != nil {
			t.Fatal(err)
		}
		calls := m.requests()
		want := fantasy.Message{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{
			fantasy.TextPart{Text: "look at [Image #1]"},
			fantasy.TextPart{Text: "[Image omitted: Model A does not accept images. File: " + ref.Path + "]"},
		}}
		if got := lastMessage(t, calls[0]); !reflect.DeepEqual(got, want) {
			t.Fatalf("the model was sent %#v; want the words, then the placeholder", got)
		}
		noEnvelope(t, calls[0])
		stored := fantasy.NewUserMessage("look at [Image #1]", fantasy.FilePart{Filename: ref.Path, MediaType: "image/png", Data: data})
		if !reflect.DeepEqual(storedUser(t, f), stored) {
			t.Fatal("the transcript does not keep the image the model was not sent")
		}
	})

	t.Run("an image the host will not read", func(t *testing.T) {
		f := newNativeFixture(t)
		f.withVision("test/a")
		s := f.started(Options{})
		m := f.models["test/a"]
		m.push(answer("ok"))
		ref := AttachmentRef{N: 1, Path: "/h/attachments/0123456789abcdef.png", MIME: "image/png"}
		if _, err := s.Prompt(context.Background(), AttachmentBlock([]AttachmentRef{ref})+"look at [Image #1]"); err != nil {
			t.Fatal(err)
		}
		calls := m.requests()
		want := fantasy.NewUserMessage("[Image #1: /h/attachments/0123456789abcdef.png (not attached: no longer available)]\nlook at [Image #1]")
		if got := lastMessage(t, calls[0]); !reflect.DeepEqual(got, want) {
			t.Fatalf("the model was sent %#v; want the path text with its reason, then the words, and no file part", got)
		}
		noEnvelope(t, calls[0])
	})
}

// TestNativeLoadReplaysAnImagePromptAsItsChip: a restored session's user row
// for a prompt that carried an image is the prompt's words, chip and all —
// the text part the transcript keeps (replayedPrompt) — never the image or
// the envelope.
func TestNativeLoadReplaysAnImagePromptAsItsChip(t *testing.T) {
	f := newNativeFixture(t)
	f.withVision("test/a")
	ws := t.TempDir()
	ref, _ := savedImage(t, 1, 40, 30)
	s := f.started(Options{Workspace: ws})
	f.models["test/a"].push(answer("a grey square"))
	if _, err := s.Prompt(context.Background(), AttachmentBlock([]AttachmentRef{ref})+"look at [Image #1]"); err != nil {
		t.Fatal(err)
	}
	deltaSettled(t, s)
	id := s.Snapshot().SessionID
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	load := f.session(Options{Workspace: ws, LoadSessionID: id})
	evs, err := startLoad(t, load)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	lines := loadLines(evs)
	if !slices.Contains(lines, "user: look at [Image #1] [r]") || !slices.Contains(lines, "text: a grey square [r]") {
		t.Fatalf("the load published:\n%s\nwant the prompt's row showing its chip, then its answer", strings.Join(lines, "\n"))
	}
	for _, ev := range evs {
		if strings.Contains(ev.Text, "craze_attachments") || strings.Contains(ev.Text, ref.Path) {
			t.Fatalf("a replayed event carries the envelope or the image's path: %q", ev.Text)
		}
	}
}
