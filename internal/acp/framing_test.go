package acp

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestFraming(t *testing.T) {
	t.Run("two messages in one read", func(t *testing.T) {
		msg1 := `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`
		msg2 := `{"jsonrpc":"2.0","id":2,"result":{"ok":false}}`
		d := NewDecoder(bytes.NewReader([]byte(msg1 + "\n" + msg2 + "\n")))
		a, err := d.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		b, err := d.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if string(a.ID) != "1" || string(b.ID) != "2" {
			t.Fatalf("ids %s %s", a.ID, b.ID)
		}
	})

	t.Run("partial line", func(t *testing.T) {
		pr, pw := io.Pipe()
		d := NewDecoder(pr)
		full := `{"jsonrpc":"2.0","id":7,"result":{}}` + "\n"
		errCh := make(chan error, 1)
		var got *Message
		go func() {
			m, err := d.ReadMessage()
			got = m
			errCh <- err
		}()
		if _, err := pw.Write([]byte(full[:10])); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-errCh:
			t.Fatalf("decoded before remainder: %v", err)
		case <-time.After(30 * time.Millisecond):
		}
		if _, err := pw.Write([]byte(full[10:])); err != nil {
			t.Fatal(err)
		}
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
		if string(got.ID) != "7" {
			t.Fatalf("id %s", got.ID)
		}
		_ = pw.Close()
	})

	t.Run("line larger than 64KiB", func(t *testing.T) {
		text := strings.Repeat("x", 70*1024)
		payload := `{"jsonrpc":"2.0","id":1,"result":{"text":"` + text + `"}}` + "\n"
		d := NewDecoder(strings.NewReader(payload))
		msg, err := d.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(msg.Result, []byte(text[:64])) {
			t.Fatal("missing large payload")
		}
	})
}

func TestRejectsContentLengthFraming(t *testing.T) {
	inputs := []string{
		"Content-Length: 32\r\n\r\n{\"jsonrpc\":\"2.0\",\"id\":1}\n",
		"content-length: 2\n\n{}\n",
		"Content-Length: 0\r\n\r\n",
	}
	for _, in := range inputs {
		d := NewDecoder(strings.NewReader(in))
		_, err := d.ReadMessage()
		if !errors.Is(err, ErrContentLengthFraming) {
			t.Fatalf("input %q: got %v, want ErrContentLengthFraming", in, err)
		}
		if err == nil || !strings.Contains(err.Error(), "Content-Length") {
			t.Fatalf("error should mention Content-Length: %v", err)
		}
	}
}

func TestPickYoloAllowUsesRequestOptionID(t *testing.T) {
	opts := []PermissionOption{
		{OptionID: "yes-this-time", Name: "Yes", Kind: KindAllowOnce},
		{OptionID: "no-thanks", Name: "No", Kind: KindRejectOnce},
	}
	id, ok := PickYoloAllow(opts)
	if !ok || id != "yes-this-time" {
		t.Fatalf("got %q %v", id, ok)
	}
	if optionIDInRequest(opts, "allow_once") {
		t.Fatal("must not invent allow_once")
	}

	prefer := []PermissionOption{
		{OptionID: "opt-once", Name: "Once", Kind: KindAllowOnce},
		{OptionID: "opt-always", Name: "Always", Kind: KindAllowAlways},
	}
	id, ok = PickYoloAllow(prefer)
	if !ok || id != "opt-always" {
		t.Fatalf("yolo should prefer allow_always, got %q %v", id, ok)
	}
}
