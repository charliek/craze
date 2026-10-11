// Package http2 is a test fixture for messagesapi: an error type in a
// package whose import path ends in /http2, as golang.org/x/net/http2's
// does, so messagesapi's tests can show its transport classification reads
// an HTTP/2 package's type even when the error's text names nothing (review
// c1r3 2). Nothing but those tests imports it.
package http2

import "fmt"

// ResetError is a failure of the kind an HTTP/2 package reports, its text
// free of every word a text check looks for.
type ResetError struct{ Code uint32 }

func (e ResetError) Error() string { return fmt.Sprintf("peer reset the exchange (code %d)", e.Code) }
