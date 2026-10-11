package acp

import (
	"context"
	"errors"
	"strings"
)

// ErrEmptyAnswer is a side question grok answered with no text.
var ErrEmptyAnswer = errors.New("acp: the side question got an empty answer")

// BtwParams is x.ai/btw. content (image blocks) is never sent: a side question
// is text only.
type BtwParams struct {
	SessionID string `json:"sessionId"`
	Question  string `json:"question"`
}

// btwAck is the reply, nested one level under the JSON-RPC result:
// {"result":{"result":{"answer":"…"}}} as the client sees it (verified live on
// grok 1.0.50 and gx 1.0.45).
type btwAck struct {
	Result struct {
		Answer string `json:"answer"`
	} `json:"result"`
}

// Aside asks one side question of the running session and returns the answer.
// Only the grok dialect has it: every other dialect returns ErrUnsupported
// before anything reaches the wire, and so does an agent that answers -32601
// (an older grok). Cancelling ctx only abandons the wait; grok keeps
// generating.
func (c *Client) Aside(ctx context.Context, question string) (string, error) {
	c.mu.Lock()
	d := c.dialect
	sid := c.sessionID
	c.mu.Unlock()
	if d != DialectGrok {
		return "", ErrUnsupported
	}
	if sid == "" {
		return "", ErrNoSession
	}
	var ack btwAck
	if err := c.conn.Call(ctx, MethodGrokBtwWrapped, BtwParams{SessionID: sid, Question: question}, &ack); err != nil {
		var re *RPCError
		if errors.As(err, &re) && re.Code == CodeMethodNotFound {
			return "", ErrUnsupported
		}
		return "", err
	}
	if strings.TrimSpace(ack.Result.Answer) == "" {
		return "", ErrEmptyAnswer
	}
	return ack.Result.Answer, nil
}
