package tool

import (
	"bytes"
	"context"
	"testing"
)

// TestDispatcherTellsCallsTheTurnsModel: every Env the dispatcher hands a
// Prepare and a Run says what SetVision last said — whether the model accepts
// images, and its name — so a turn's calls see its model, and the next turn's
// calls the model a /model switch chose (plan 033 §3.5). Before any SetVision
// a call is told no vision and no name, which ModelLabel words "this model".
func TestDispatcherTellsCallsTheTurnsModel(t *testing.T) {
	type seen struct {
		vision bool
		name   string
	}
	var prepared, ran seen
	f := newFake("echo", func(_ context.Context, env Env, _ fakeInput) Result {
		ran = seen{env.Vision, env.ModelName}
		return Result{Text: "ok"}
	})
	f.req = func(_ fakeInput, env Env) Request {
		prepared = seen{env.Vision, env.ModelName}
		return Request{}
	}
	d := newDispatcher(t, testEnv(t), nil, f)
	call := func(id string) {
		t.Helper()
		if _, _, ok := d.Prepare(Call{ID: id, Tool: "echo", Input: input(t, fakeInput{Text: "x"})}); !ok {
			t.Fatal("Prepare refused the call")
		}
		d.Run(context.Background(), id, nil)
	}

	call("t1.1.1")
	if prepared != (seen{}) || ran != (seen{}) {
		t.Fatalf("before any SetVision the call saw %+v / %+v; want no vision and no name", prepared, ran)
	}
	if got := (Env{}).ModelLabel(); got != "this model" {
		t.Fatalf("ModelLabel with no name = %q", got)
	}
	for i, want := range []seen{{true, "Eye 1"}, {false, "GLM 5.3 (Z.AI)"}, {true, "Eye 2"}} {
		d.SetVision(want.vision, want.name)
		call("t2.1." + string(rune('1'+i)))
		if prepared != want || ran != want {
			t.Fatalf("after SetVision(%v, %q) Prepare saw %+v and Run %+v", want.vision, want.name, prepared, ran)
		}
	}
	if got := (Env{ModelName: "GLM"}).ModelLabel(); got != "GLM" {
		t.Fatalf("ModelLabel = %q, want the name", got)
	}
}

// TestDispatcherKeepsAnImageOnlyWhereOneMayGo (plan 033 §3.5): a successful
// result's image is kept on a turn whose model accepts images, untouched;
// an error's is dropped; and on a turn whose model does not accept images the
// image is dropped and the text ends with the line the harness's vision strip
// gives a tool's image — so no tool, whether or not it honours Env.Vision,
// gets an image to that model from the turn's own steps.
func TestDispatcherKeepsAnImageOnlyWhereOneMayGo(t *testing.T) {
	img := []byte("\x89PNG\r\n\x1a\n not really, but bytes")
	cases := []struct {
		name      string
		vision    bool
		res       Result
		wantMedia bool
		wantText  string
	}{
		{"vision keeps it", true, Result{Text: "Read image file: a.png"}, true, "Read image file: a.png"},
		{"an error never carries one", true, Result{Text: "no", IsError: true}, false, "no"},
		{"no vision drops it and says so", false, Result{Text: "Read image file: a.png"}, false,
			"Read image file: a.png\n[Image omitted: GLM does not accept images]"},
		{"no vision and no text", false, Result{}, false, "[Image omitted: GLM does not accept images]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := tc.res
			res.Media = &Media{Data: img, MIME: "image/png"}
			d := newDispatcher(t, testEnv(t), nil, newFake("echo", func(context.Context, Env, fakeInput) Result { return res }))
			d.SetVision(tc.vision, "GLM")
			d.Prepare(Call{ID: "t1.1.1", Tool: "echo", Input: input(t, fakeInput{Text: "x"})})
			got := d.Run(context.Background(), "t1.1.1", nil)
			if (got.Media != nil) != tc.wantMedia || got.Text != tc.wantText {
				t.Fatalf("Run = text %q, media %v; want text %q, media %v", got.Text, got.Media != nil, tc.wantText, tc.wantMedia)
			}
			if got.Media != nil && (!bytes.Equal(got.Media.Data, img) || got.Media.MIME != "image/png") {
				t.Fatalf("the kept image = %d bytes of %q; want the tool's, untouched", len(got.Media.Data), got.Media.MIME)
			}
		})
	}
}
