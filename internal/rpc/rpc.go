package rpc

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
)

type ReaderFunc func(ctx context.Context, req *Request, errRpcError bool) (*Response, error)

var BatchIDCounter atomic.Uint64

// GetRequestIDString returns the request ID as a string.
// The request ID is commonly a number, but might be a string with a number or a string.
func GetRequestIDString(id any) string {
	switch v := id.(type) {
	case string:
		return v
	case float64:
		return strconv.Itoa(int(v))
	}

	return ""
}

// FormatRawBody formats a response body so we can include it in log output in a human readable format
func FormatRawBody(body string) string {
	if len(body) > 64 {
		body = body[:64] + "... (truncated)"
	}

	body = strings.ReplaceAll(body, "\n", " ")

	return body
}
