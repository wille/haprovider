package rpc

import (
	"strings"
	"sync/atomic"
)

var BatchIDCounter atomic.Uint64

// maxLogBodyLen is how many characters of a raw body we keep in log output
// before truncating.
const maxLogBodyLen = 512

// FormatRawBody formats a response body so we can include it in log output in a human readable format
func FormatRawBody(body string) string {
	if len(body) > maxLogBodyLen {
		body = body[:maxLogBodyLen] + "... (truncated)"
	}

	body = strings.ReplaceAll(body, "\n", " ")
	body = strings.TrimSpace(body)

	return body
}
