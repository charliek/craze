package protocol_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/protocol"
)

// TestProtocolReferenceNamesEveryMethodNotificationCodeAndReason keeps
// docs/reference/protocol.md current: every method, notification, error code,
// error reason and JSON-RPC error integer the package declares is named in it, in backticks.
func TestProtocolReferenceNamesEveryMethodNotificationCodeAndReason(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(moduleRoot(t), "docs", "reference", "protocol.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	check := func(kind, name string) {
		t.Helper()
		if !strings.Contains(doc, "`"+name+"`") {
			t.Errorf("docs/reference/protocol.md does not name %s `%s`", kind, name)
		}
	}
	for _, m := range protocol.Methods() {
		check("method", m.Name)
	}
	for _, n := range protocol.Notifications() {
		check("notification", n)
	}
	for _, c := range protocol.Codes() {
		check("error code", string(c))
	}
	for _, r := range protocol.Reasons() {
		check("error reason", string(r.Reason))
	}
	for _, n := range []int{protocol.RPCParseError, protocol.RPCInvalidRequest, protocol.RPCMethodNotFound, protocol.RPCInvalidParams, protocol.RPCRefused} {
		check("JSON-RPC error code", strconv.Itoa(n))
	}
}
