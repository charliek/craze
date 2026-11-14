package agent

import (
	"reflect"
	"strings"
	"testing"
)

// TestTheWebSearchSectionRoundTrips (plan 051 F.4.3): a state delta's
// webSearch section crosses the event codec whole — every member, the URL
// list in order — and is written under `webSearch` with its members' wire
// names; a decoded section shares no URL list with the body or the event it
// was encoded from.
func TestTheWebSearchSectionRoundTrips(t *testing.T) {
	urls := []string{"https://a.example", "https://b.example/ü"}
	ev := Event{Type: EventMeta, State: &StateDelta{WebSearch: &WebSearchState{
		Searches: 3, Fetches: 2, Failed: 1, URLs: urls, URLsTruncated: true,
	}}}
	body, err := EncodeEvent(ev)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"state":{"webSearch":{"searches":3,"fetches":2,"failed":1,"urls":["https://a.example","https://b.example/ü"],"urlsTruncated":true}}`) {
		t.Fatalf("the wire shape: %s", body)
	}
	got, err := DecodeEvent(body)
	if err != nil {
		t.Fatal(err)
	}
	if got.State == nil || !reflect.DeepEqual(got.State.WebSearch, ev.State.WebSearch) {
		t.Fatalf("decoded %+v, want %+v", got.State, ev.State.WebSearch)
	}
	got.State.WebSearch.URLs[0] = "https://scribbled.example"
	if urls[0] != "https://a.example" {
		t.Fatal("the decoded section shares its URL list with the encoded event")
	}
}

// TestTheWebSearchSectionOmitsWhatIsZero: each member is omitted when zero,
// empty or false; a section that counts nothing is `{}` (present: it
// replaces), and a delta with no section carries no key at all.
func TestTheWebSearchSectionOmitsWhatIsZero(t *testing.T) {
	for _, c := range []struct {
		name string
		st   *WebSearchState
		want string
	}{
		{"empty", &WebSearchState{}, `"state":{"webSearch":{}}`},
		{"searches alone", &WebSearchState{Searches: 1}, `"state":{"webSearch":{"searches":1}}`},
		{"an empty list", &WebSearchState{Fetches: 1, URLs: []string{}}, `"state":{"webSearch":{"fetches":1}}`},
	} {
		body, err := EncodeEvent(Event{Type: EventMeta, State: &StateDelta{WebSearch: c.st}})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(body, c.want) {
			t.Errorf("%s: %s, want %s", c.name, body, c.want)
		}
		got, err := DecodeEvent(body)
		if err != nil {
			t.Fatal(err)
		}
		if got.State.WebSearch == nil {
			t.Errorf("%s: a present section decoded as absent", c.name)
		}
	}
	title := "t"
	body, err := EncodeEvent(Event{Type: EventMeta, State: &StateDelta{Title: &title}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "webSearch") {
		t.Fatalf("a delta with no section carries one: %s", body)
	}
	got, err := DecodeEvent(body)
	if err != nil {
		t.Fatal(err)
	}
	if got.State.WebSearch != nil {
		t.Fatal("an absent section decoded as present")
	}
}

// TestWebSearchStateCloneSharesNothing: Clone's URL list is its own, nil for
// nil; Empty is true only for a tally that counts nothing.
func TestWebSearchStateCloneSharesNothing(t *testing.T) {
	var none *WebSearchState
	if none.Clone() != nil {
		t.Fatal("a nil tally's clone is not nil")
	}
	w := &WebSearchState{Searches: 1, URLs: []string{"https://a.example"}}
	c := w.Clone()
	c.URLs[0] = "x"
	if w.URLs[0] != "https://a.example" {
		t.Fatal("a clone shares its URL list")
	}
	if !(WebSearchState{}).Empty() {
		t.Fatal("the zero tally is not empty")
	}
	for _, v := range []WebSearchState{{Searches: 1}, {Fetches: 1}, {Failed: 1}, {URLs: []string{"u"}}, {URLsTruncated: true}} {
		if v.Empty() {
			t.Errorf("%+v reads as empty", v)
		}
	}
}
