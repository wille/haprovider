package rpc

import (
	"context"
	"strings"
	"sync/atomic"
)

type ReaderFunc func(ctx context.Context, req *Request, errRpcError bool) (*Response, error)

var BatchIDCounter atomic.Uint64

// FormatRawBody formats a response body so we can include it in log output in a human readable format
func FormatRawBody(body string) string {
	if len(body) > 128 {
		body = body[:128] + "... (truncated)"
	}

	body = strings.ReplaceAll(body, "\n", " ")
	body = strings.TrimSpace(body)

	return body
}
