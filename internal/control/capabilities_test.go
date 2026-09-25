package control_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/control"
	"github.com/charliek/craze/internal/protocol"
)

// TestEveryCapabilityIsOnTheWire (plan 027 §3.3): every agent.Capabilities
// field — found by reflection, so a field a later phase adds (H6 PR 3's
// SubagentBackground, H7's) fails the gate until it is mapped — is carried by
// the server's mapping under exactly one wire name of its own, and that name is
// a required property of the schema's session capability set
// (info.json#/$defs/sessionCapabilities). The four the protocol states for
// every host — cancel, approvals, historyCursor true, and stop false on a TUI
// host — are set whatever the provider says.
func TestEveryCapabilityIsOnTheWire(t *testing.T) {
	props, required := schemaCapabilities(t)
	constant := map[string]bool{"cancel": true, "approvals": true, "historyCursor": true, "stop": false}
	wireOf := func(c agent.Capabilities) map[string]bool {
		t.Helper()
		b, err := protocol.MarshalLine(control.SessionCapabilities(c))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]bool
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	for name, want := range constant {
		if got := wireOf(agent.Capabilities{})[name]; got != want {
			t.Errorf("%s is %v on the wire, want %v for every host", name, got, want)
		}
	}
	typ := reflect.TypeFor[agent.Capabilities]()
	seen := map[string]string{}
	for i := range typ.NumField() {
		f := typ.Field(i)
		if f.Type.Kind() != reflect.Bool {
			t.Errorf("agent.Capabilities.%s is a %s: the wire carries capabilities as booleans", f.Name, f.Type)
			continue
		}
		var c agent.Capabilities
		reflect.ValueOf(&c).Elem().Field(i).SetBool(true)
		var on []string
		for k, v := range wireOf(c) {
			if _, fixed := constant[k]; v && !fixed {
				on = append(on, k)
			}
		}
		if len(on) != 1 {
			t.Errorf("agent.Capabilities.%s set alone turns on %v on the wire, want exactly one wire name of its own: map it in sessionCapabilities (info.go), in protocol.SessionCapabilities and in info.json's sessionCapabilities", f.Name, on)
			continue
		}
		wire := on[0]
		if other, dup := seen[wire]; dup {
			t.Errorf("agent.Capabilities.%s and .%s share the wire name %s", f.Name, other, wire)
		}
		seen[wire] = f.Name
		if !props[wire] || !slices.Contains(required, wire) {
			t.Errorf("agent.Capabilities.%s's wire name %s is not a required property of info.json's sessionCapabilities", f.Name, wire)
		}
	}
	// And nothing on the wire is neither a mapped field nor a constant.
	for wire := range wireOf(agent.Capabilities{}) {
		if _, fixed := constant[wire]; !fixed && seen[wire] == "" {
			t.Errorf("the wire carries %s, which no agent.Capabilities field sets", wire)
		}
	}
}

// schemaCapabilities is info.json's sessionCapabilities: its properties and
// its required list.
func schemaCapabilities(t *testing.T) (map[string]bool, []string) {
	t.Helper()
	b, err := protocol.SchemaFile(protocol.SchemaInfo)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Defs map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	def, ok := doc.Defs["sessionCapabilities"]
	if !ok {
		t.Fatal("info.json has no sessionCapabilities")
	}
	props := map[string]bool{}
	for k := range def.Properties {
		props[k] = true
	}
	return props, def.Required
}
