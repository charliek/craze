package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/harness/llm/messagesapi"
	"github.com/charliek/craze/internal/harness/redact"
	"github.com/charliek/craze/internal/harness/tool"
)

// The key in a reply's text (plan 051 §B, R1, R2): the caller's own key comes
// out of every text a reply gives — an error's and a success's, titles,
// content and URLs — in every spelling an echo can give it, before anything
// parses, classifies or cuts the text.

// weirdKey is a usable key holding every character a URL or JSON spells
// differently: "+", "/", "%", "&", a quote and spaces.
const weirdKey = `exa key+with/odd%chars&"more 0001`

// unicodeEscaped is v with every character a JSON \u escape, as a server that
// escapes everything echoes it.
func unicodeEscaped(v string) string {
	var b strings.Builder
	for _, r := range v {
		fmt.Fprintf(&b, `\u%04x`, r)
	}
	return b.String()
}

// splitBy is v with sep between its halves.
func splitBy(v, sep string) string { return v[:len(v)/2] + sep + v[len(v)/2:] }

// echoes are the spellings of key a server's text can carry it in.
func echoes(key string) map[string]string {
	marshalled, _ := json.Marshal(key) // "&" as &, the quote as \"
	plus := strings.ReplaceAll(key, " ", "+")
	return map[string]string{
		"as itself":                           key,
		"JSON-escaped as Go escapes it":       string(marshalled[1 : len(marshalled)-1]),
		"every character a \\u escape":        unicodeEscaped(key),
		"query-encoded":                       url.QueryEscape(key),
		"path-encoded":                        url.PathEscape(key),
		"spaces as + with a zero-width space": splitBy(plus, "\u200b"),
		"split by a zero-width space":         splitBy(key, "\u200b"),
		"split by a soft hyphen":              splitBy(key, "\u00ad"),
		"split by an invalid byte":            splitBy(key, "\xff"),
		"split by U+FFFD":                     splitBy(key, "\ufffd"),
	}
}

// spellings are the forms of key a check looks for in text a display shows
// (shown): what an echo leaves once format characters and bad bytes are
// dropped.
func spellings(key string) []string {
	marshalled, _ := json.Marshal(key)
	return []string{key, string(marshalled[1 : len(marshalled)-1]), unicodeEscaped(key), url.QueryEscape(key),
		url.PathEscape(key), strings.ReplaceAll(key, " ", "+")}
}

// carriesKey says text, as a display shows it, holds key in any spelling, or
// its first half — what a cut through it would leave.
func carriesKey(text, key string) string {
	v := shown(text)
	for _, s := range spellings(key) {
		if strings.Contains(v, s) {
			return s
		}
	}
	if strings.Contains(v, key[:len(key)/2]) {
		return key[:len(key)/2]
	}
	return ""
}

// TestErrorsCarryNoKey (R2): a key with "+", "/", "%", "&", a quote and
// spaces, echoed in a JSON-RPC error's message or an error result's text in
// any spelling — as itself, JSON-escaped, percent-encoded, a query string's
// "+" for a space, split by a zero-width space, a soft hyphen or an invalid
// byte — is in no error the service returns, search's or fetch's; the
// marker stands where it was, so the provider's words did come through (the
// control). An error result whose text names a status is classified with
// the key gone too.
func TestErrorsCarryNoKey(t *testing.T) {
	for name, echo := range echoes(weirdKey) {
		t.Run(name, func(t *testing.T) {
			for kind, reply := range map[string]replyFunc{
				"a JSON-RPC error": func(w http.ResponseWriter, _ *http.Request, rec received) {
					sendJSON(w, errorJSON(rec.Call.ID, "bad key "+echo+" given"))
				},
				"an error result": func(w http.ResponseWriter, _ *http.Request, rec received) {
					sendSSE(w, event(resultJSON(rec.Call.ID, true, "bad key "+echo+" given")))
				},
				"an error result with a status": func(w http.ResponseWriter, _ *http.Request, rec received) {
					sendSSE(w, event(resultJSON(rec.Call.ID, true, "web_search_exa error (401): bad key "+echo)))
				},
			} {
				f := newFake(t, reply)
				if kind != "an error result with a status" {
					// The control: with no key to scrub, the words carry the
					// echo, as a display shows it.
					_, err := search(overridden(t, f, ""), "q")
					if text := webErr(t, err).Text; !strings.Contains(shown(text), shown(valid(echo))[:20]) {
						t.Fatalf("control: %s: %q does not carry the echo", kind, text)
					}
				}
				s := keyed(t, f, weirdKey, 0)
				_, serr := search(s, "q")
				_, ferr := s.Fetch(context.Background(), "https://a.example/")
				for op, err := range map[string]error{"search": serr, "fetch": ferr} {
					text := webErr(t, err).Text
					if found := carriesKey(text, weirdKey); found != "" {
						t.Errorf("%s, %s: %q carries the key as %q", kind, op, text, found)
					}
					if kind == "an error result with a status" {
						if text != "Web search authentication failed (HTTP 401)" {
							t.Errorf("%s, %s: %q", kind, op, text)
						}
					} else if !strings.Contains(text, " bad key "+redact.Marker+" given") {
						t.Errorf("%s, %s: %q does not show the scrubbed words", kind, op, text)
					}
				}
			}
		})
	}
}

// TestScrubRunsBeforeTheCut (R2): a provider's words are cut to maxDetail
// bytes only after the key is out, so a key that straddles the cut leaves no
// part of itself behind. The control: the same words cut unscrubbed keep the
// key's first half.
func TestScrubRunsBeforeTheCut(t *testing.T) {
	msg := strings.Repeat("a", maxDetail-len(weirdKey)/2) + weirdKey + " tail"
	if unscrubbed := cut(msg, maxDetail); !strings.Contains(unscrubbed, weirdKey[:len(weirdKey)/2]) {
		t.Fatalf("control: the key does not straddle the cut: %q", unscrubbed)
	}
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, rec received) {
		sendJSON(w, errorJSON(rec.Call.ID, msg))
	})
	_, err := search(keyed(t, f, weirdKey, 0), "q")
	text := webErr(t, err).Text
	if found := carriesKey(text, weirdKey); found != "" {
		t.Fatalf("%q carries the key as %q", text, found)
	}
	if !strings.HasSuffix(text, "…") || !strings.Contains(text, "a"+redact.Marker[:5]) {
		t.Fatalf("%q: the cut did not fall inside the marker", text)
	}
}

// TestScrubRunsBeforeTheClassifying (R2): an error result whose only status
// is inside the key is not read as that status — the key is gone before the
// text is read for one — and one whose block structure is inside the key
// gives no result from it.
func TestScrubRunsBeforeTheClassifying(t *testing.T) {
	const statusKey = "zz-error (429)-key-0001"
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, rec received) {
		sendSSE(w, event(resultJSON(rec.Call.ID, true, "rejected "+statusKey)))
	})
	_, err := search(keyed(t, f, statusKey, 0), "q")
	wantErr(t, err, tool.ClassToolError, "Unable to search the web for q: rejected "+redact.Marker)

	const blockKey = "k\n---\nTitle: K\nURL: https://k.example/\nHighlights:\nkey"
	f = newFake(t, replyText("Title: A\nURL: https://a.example/\nHighlights:\nsee "+blockKey))
	// A key with line breaks cannot go in a header: under the override it
	// is not sent, and is scrubbed all the same.
	results, err := search(overridden(t, f, blockKey), "q")
	if err != nil || len(results) != 1 || results[0].Content != "see "+redact.Marker {
		t.Fatalf("Search = %+v, %v; want the one result, the key scrubbed from it", results, err)
	}
}

// TestSuccessfulRepliesAreScrubbed (R1): the key comes out of a successful
// search's titles, content and URLs and out of a fetched page, in every
// spelling — a key straddling the 500-character URL cut and the dispatcher's
// long-line cut (tool.MaxBytes) included, so no cut the tools make later can
// leave half of it. The controls: the reply does carry the key there, and
// cut unscrubbed at those points keeps its first half.
func TestSuccessfulRepliesAreScrubbed(t *testing.T) {
	const urlCut = 500
	key := weirdKey
	half := key[:len(key)/2]
	esc := echoes(key)
	longURL := "https://a.example/?q=" + strings.Repeat("u", urlCut-len("https://a.example/?q=")-len(half)) + key + "&x=1"
	longLine := strings.Repeat("l", tool.MaxBytes-len(half)) + key + " end"
	searchText := "Title: about " + key + "\nURL: https://b.example/?k=" + esc["query-encoded"] + "\nHighlights:\n" +
		esc["split by a zero-width space"] + " and " + esc["every character a \\u escape"] + " and " + esc["split by an invalid byte"] +
		"\n---\nTitle: long\nURL: " + longURL + "\nHighlights:\n" + longLine
	pageHead := "# Page\nURL: https://c.example/" + esc["path-encoded"] + "\n\n"
	pageText := pageHead + strings.Repeat("l", tool.MaxBytes-len(pageHead)-len(half)) + key + " end"
	if !strings.Contains(longURL[:urlCut], half) || !strings.Contains(longLine[:tool.MaxBytes], half) || !strings.Contains(pageText[:tool.MaxBytes], half) {
		t.Fatal("control: the key does not straddle the cuts")
	}

	f := newFake(t, replyText(searchText))
	results, err := search(keyed(t, f, key, 0), "q")
	if err != nil || len(results) != 2 {
		t.Fatalf("Search = %d results, %v", len(results), err)
	}
	for i, r := range results {
		for field, v := range map[string]string{"title": r.Title, "URL": r.URL, "content": r.Content} {
			if found := carriesKey(v, key); found != "" {
				t.Errorf("result %d's %s carries the key as %q", i, field, found)
			}
		}
	}
	if r := results[0]; !strings.Contains(r.Title, redact.Marker) || !strings.Contains(r.URL, redact.Marker) || strings.Count(r.Content, redact.Marker) != 3 {
		t.Errorf("control: result 0 = %+v; want the marker in its title, URL and three times in its content", r)
	}
	if long := results[1].URL; carriesKey(long[:min(urlCut, len(long))], key) != "" || !strings.Contains(long, redact.Marker) {
		t.Errorf("the long URL cut at %d carries the key", urlCut)
	}
	if c := results[1].Content; carriesKey(c[:min(tool.MaxBytes, len(c))], key) != "" || !strings.Contains(c, redact.Marker) {
		t.Errorf("the long line cut at %d carries the key", tool.MaxBytes)
	}

	f = newFake(t, replyText(pageText))
	page, err := keyed(t, f, key, 0).Fetch(context.Background(), "https://c.example/")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if found := carriesKey(page.Text, key); found != "" {
		t.Errorf("the page carries the key as %q", found)
	}
	if strings.Count(page.Text, redact.Marker) != 2 || carriesKey(page.Text[:min(tool.MaxBytes, len(page.Text))], key) != "" {
		t.Errorf("the page = %.80q…; want the marker twice and nothing of the key at the cut", page.Text)
	}
}

// TestScrubber (R2): the scrub of one key, as the Messages API client's
// pattern reads it — format characters and invalid bytes between its
// characters are nothing, a space is no gap, part of the key and ordinary
// text are left alone — with redact.Marker in its place; the query string's
// "+" for a space too. The key is trimmed before anything, and a key that
// is not usable (blank, too short, overlapping the marker) builds no
// scrubber, so an empty pattern never matches everywhere; text is made valid
// either way.
func TestScrubber(t *testing.T) {
	const metaKey = `sk-(a.b)*+?[c]{2}|^$\x-0001`
	for _, key := range []string{"exa-0123456789abcdef", metaKey} {
		h := len(key) / 2
		s := newScrubber("  " + key + "\t")
		client := messagesapi.KeyPattern(key)
		for _, tc := range []struct{ name, in, want string }{
			{"a zero-width space", "k " + key[:h] + "\u200b" + key[h:] + ".", "k " + redact.Marker + "."},
			{"a soft hyphen", key[:h] + "\u00ad" + key[h:], redact.Marker},
			{"an invalid byte", key[:h] + "\xff" + key[h:], redact.Marker},
			{"before and after stand", "\u200b" + key + "\u00ad", "\u200b" + redact.Marker + "\u00ad"},
			{"a space is no gap", key[:h] + " " + key[h:], key[:h] + " " + key[h:]},
			{"a part of the key", key[:h] + "\u200b", key[:h] + "\u200b"},
			{"ordinary text", "soft\u00adhyphen, zero\u200bwidth", "soft\u00adhyphen, zero\u200bwidth"},
		} {
			if got := s.text(tc.in); got != tc.want {
				t.Errorf("%s: text(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
			}
			if same := client.ReplaceAllLiteralString(valid(tc.in), redact.Marker); s.text(tc.in) != same {
				t.Errorf("%s: the scrub differs from the client's pattern: %q", tc.name, same)
			}
		}
	}
	if got := newScrubber("exa key 0001").text("?k=exa+k\u200bey+0001&x"); got != "?k="+redact.Marker+"&x" {
		t.Errorf("the query spelling of a key with spaces scrubs to %q", got)
	}
	for _, unusable := range []string{"", "   ", "short", " 1234567 ", "redacted", redact.Marker, "credential]-0001"} {
		if newScrubber(unusable) != nil {
			t.Errorf("%q built a scrubber", unusable)
		}
	}
	var none *scrubber
	if got := none.text("a\xffb\ufffdc"); got != "abc" {
		t.Errorf("no scrubber: text = %q, want the text made valid", got)
	}
}

// mixedKey is a usable key with spaces, for the spellings in which each of its
// spaces is spelled on its own.
const mixedKey = "alpha beta gamma"

// mixedEchoes are spellings of mixedKey whose spaces are each spelled
// differently \u2014 a raw space, a query string's "+", %20, a JSON \u escape \u2014
// some with a format character at a space's edge.
var mixedEchoes = map[string]string{
	"+ then %20":                    "alpha+beta%20gamma",
	"%20 then +":                    "alpha%20beta+gamma",
	"+ then a raw space":            "alpha+beta gamma",
	"a JSON escape then +":          "alpha" + `\` + "u0020beta+gamma",
	"a zero-width space after +":    "alpha+\u200bbeta%20gamma",
	"a zero-width space before %20": "alpha\u200b%20beta+gamma",
	"a soft hyphen after %20":       "alpha%20\u00adbeta gamma",
}

// TestMixedSpaceSpellings (R2): a key with spaces is found however each of its
// spaces is spelled, independently of the others \u2014 in the scrub itself, in an
// error's detail, and in a successful search's title, URL, published date and
// content and a fetched page.
func TestMixedSpaceSpellings(t *testing.T) {
	s := newScrubber(mixedKey)
	for name, echo := range mixedEchoes {
		t.Run(name, func(t *testing.T) {
			if got, want := s.text("x "+echo+" y"), "x "+redact.Marker+" y"; got != want {
				t.Fatalf("text(%q) = %q, want %q", echo, got, want)
			}

			f := newFake(t, func(w http.ResponseWriter, _ *http.Request, rec received) {
				sendJSON(w, errorJSON(rec.Call.ID, "bad key "+echo+" given"))
			})
			_, err := search(keyed(t, f, mixedKey, 0), "q")
			wantErr(t, err, tool.ClassToolError, "Unable to search the web for q: bad key "+redact.Marker+" given")

			f = newFake(t, replyText("Title: about "+echo+"\nURL: https://b.example/?k="+echo+
				"\nPublished: "+echo+"\nHighlights:\n"+echo+" end"))
			results, err := search(keyed(t, f, mixedKey, 0), "q")
			want := tool.WebResult{Title: "about " + redact.Marker, URL: "https://b.example/?k=" + redact.Marker,
				Published: redact.Marker, Content: redact.Marker + " end"}
			if err != nil || len(results) != 1 || results[0] != want {
				t.Fatalf("Search = %+v, %v; want [%+v]", results, err, want)
			}

			f = newFake(t, replyText("# "+echo+"\nURL: https://c.example/"+echo+"\n\nbody "+echo))
			page, err := keyed(t, f, mixedKey, 0).Fetch(context.Background(), "https://c.example/")
			wantText := "# " + redact.Marker + "\nURL: https://c.example/" + redact.Marker + "\n\nbody " + redact.Marker
			if err != nil || page.Text != wantText {
				t.Fatalf("Fetch = %q, %v; want %q", page.Text, err, wantText)
			}
		})
	}
}

// TestEchoedInputsAreScrubbed (R1, R2): the query and the page's URL are text
// the service returns too \u2014 in the words of a failure and as a fetched page's
// URL \u2014 so the key comes out of them, as itself and encoded, in success and
// failure alike.
func TestEchoedInputsAreScrubbed(t *testing.T) {
	for _, key := range []string{"exa-key-0123456789abcdef", weirdKey} {
		for name, spelling := range map[string]string{
			"as itself":                   key,
			"query-encoded":               url.QueryEscape(key),
			"path-encoded":                url.PathEscape(key),
			"split by a zero-width space": splitBy(key, "\u200b"),
		} {
			t.Run(key+"/"+name, func(t *testing.T) {
				query := "about " + spelling
				pageURL := "https://example.test/?key=" + spelling
				for kind, reply := range map[string]replyFunc{
					"a malformed reply": func(w http.ResponseWriter, _ *http.Request, _ received) { sendJSON(w, "not json") },
					"a provider error": func(w http.ResponseWriter, _ *http.Request, rec received) {
						sendJSON(w, errorJSON(rec.Call.ID, "oops"))
					},
				} {
					suffix := ""
					if kind == "a provider error" {
						suffix = ": oops"
					}
					f := newFake(t, reply)
					s := keyed(t, f, key, 0)
					_, err := search(s, query)
					wantErr(t, err, tool.ClassToolError, "Unable to search the web for about "+redact.Marker+suffix)
					_, err = s.Fetch(context.Background(), pageURL)
					wantErr(t, err, tool.ClassToolError, "Unable to fetch https://example.test/?key="+redact.Marker+suffix)
				}

				// A transport failure: the endpoint is gone.
				f := newFake(t, func(http.ResponseWriter, *http.Request, received) {})
				s := keyed(t, f, key, 0)
				f.srv.Close()
				_, err := search(s, query)
				wantErr(t, err, tool.ClassToolError, "Unable to search the web for about "+redact.Marker)
				_, err = s.Fetch(context.Background(), pageURL)
				wantErr(t, err, tool.ClassToolError, "Unable to fetch https://example.test/?key="+redact.Marker)

				f = newFake(t, replyText("# P\nURL: https://example.test/\n\nbody"))
				page, err := keyed(t, f, key, 0).Fetch(context.Background(), pageURL)
				if err != nil || page.URL != "https://example.test/?key="+redact.Marker {
					t.Fatalf("Fetch = %+v, %v; want the URL with the key scrubbed", page, err)
				}
			})
		}
	}
}
