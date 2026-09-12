package acp

import (
	"encoding/json"
	"fmt"
)

const (
	jsonrpcVersion = "2.0"

	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
)

type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

func (m *Message) IsNotification() bool {
	return m != nil && m.Method != "" && len(m.ID) == 0
}

func (m *Message) IsRequest() bool {
	return m != nil && m.Method != "" && len(m.ID) > 0
}

func (m *Message) IsResponse() bool {
	return m != nil && m.Method == "" && len(m.ID) > 0
}

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return "json-rpc error"
	}
	return fmt.Sprintf("json-rpc error %d: %s", e.Code, e.Message)
}

func MethodNotFound(method string) *RPCError {
	return &RPCError{Code: CodeMethodNotFound, Message: "Method not found: " + method}
}

func marshalRaw(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	if raw, ok := v.(json.RawMessage); ok {
		return raw, nil
	}
	return json.Marshal(v)
}

func idKey(id json.RawMessage) string {
	return string(id)
}
