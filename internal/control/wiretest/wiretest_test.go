package wiretest_test

import (
	"testing"

	"github.com/charliek/craze/internal/control/wiretest"
	"github.com/charliek/craze/internal/protocol"
)

// TestTheCheckerRefusesWhatTheSchemaRefuses: the helper every wire test leans
// on is not vacuous — a line on the schema passes, and one off it (an unknown
// member, a result of the wrong method's shape, a reason under the wrong code,
// an errorResult a method does not define) fails.
func TestTheCheckerRefusesWhatTheSchemaRefuses(t *testing.T) {
	c := wiretest.Default()
	for _, tc := range []struct {
		name, method, line string
		ok                 bool
	}{
		{"a sync result", protocol.MethodSessionSync, `{"jsonrpc":"2.0","id":1,"result":{"seq":7}}`, true},
		{"a sync result with a stray member", protocol.MethodSessionSync, `{"jsonrpc":"2.0","id":1,"result":{"seq":7,"x":1}}`, false},
		{"a state result as a sync's", protocol.MethodSessionSync, `{"jsonrpc":"2.0","id":1,"result":{}}`, false},
		{"an error", protocol.MethodSessionState, `{"jsonrpc":"2.0","id":"a","error":{"code":-32000,"message":"m","data":{"code":"unknown_session","reason":"unknown_session"}}}`, true},
		{"a reason under the wrong code", protocol.MethodSessionState, `{"jsonrpc":"2.0","id":"a","error":{"code":-32000,"message":"m","data":{"code":"failed","reason":"busy"}}}`, false},
		{"a cancel's error result", protocol.MethodSessionCancel, `{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"m","data":{"code":"failed","reason":"failed","result":{"outcome":"unknown","turn":"turn-1","reported":false}}}}`, true},
		{"an error result a method does not define", protocol.MethodSessionState, `{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"m","data":{"code":"failed","reason":"failed","result":{}}}}`, false},
		{"a null id with no method", "", `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"m","data":{"code":"bad_request","reason":"bad_request"}}}`, true},
		{"a notification", "", `{"jsonrpc":"2.0","method":"reset","params":{"subscription":"s-1","reason":"session_closed"}}`, true},
		{"a notification off its schema", "", `{"jsonrpc":"2.0","method":"reset","params":{"subscription":"s-1","reason":"bored"}}`, false},
	} {
		err := c.Server(tc.method, []byte(tc.line))
		if (err == nil) != tc.ok {
			t.Errorf("%s: valid %v, want %v (%v)", tc.name, err == nil, tc.ok, err)
		}
	}
}
