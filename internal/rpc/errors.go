package rpc

import (
	"encoding/json"
	"fmt"
	"time"
)

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	// Data is the optional, provider-specific payload (e.g. an eth_call revert
	// reason). Kept raw so it's preserved exactly.
	Data json.RawMessage `json:"data,omitempty"`
}

// NewError builds a JSON-RPC error object as raw JSON, for error responses we
// generate ourselves.
func NewError(code int, message string) json.RawMessage {
	b, err := json.Marshal(RPCError{Code: code, Message: message})
	if err != nil {
		return nil
	}
	return b
}

var ErrorResponseNoProvidersAvailable = Response{
	Version: "2.0",
	ID:      nil,
	Error:   NewError(-39200, "no providers available"),
}

// Method-not-supported JSON-RPC error codes. CodeMethodNotFound is the JSON-RPC
// 2.0 standard "Method not found"; CodeMethodNotSupported is used by some
// providers (e.g. QuickNode) for "this request method is not supported".
const (
	CodeMethodNotFound     = -32601
	CodeMethodNotSupported = -32604
)

// IsMethodNotSupported reports whether a JSON-RPC error code means the provider
// does not support the requested method.
func IsMethodNotSupported(code int) bool {
	return code == CodeMethodNotFound || code == CodeMethodNotSupported
}

// MethodNotSupportedError signals that a provider reported (via a JSON-RPC error
// code) that it does not support the requested method, even when the transport
// returned a non-2xx status. The failover loop uses it to skip the provider for
// this method without marking it unhealthy.
type MethodNotSupportedError struct {
	Code    int
	Message string
}

var _ error = (*MethodNotSupportedError)(nil)

func (e *MethodNotSupportedError) Error() string {
	return fmt.Sprintf("method not supported: rpc %d: %s", e.Code, e.Message)
}

type ErrorRateLimited struct {
	Message    string
	RetryAfter time.Duration
}

var _ error = (*ErrorRateLimited)(nil)

func (e ErrorRateLimited) Error() string {
	return fmt.Sprintf("rate limited: %s", e.Message)
}
