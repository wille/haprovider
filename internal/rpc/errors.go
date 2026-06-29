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

type ErrorRateLimited struct {
	Message    string
	RetryAfter time.Duration
}

var _ error = (*ErrorRateLimited)(nil)

func (e ErrorRateLimited) Error() string {
	return fmt.Sprintf("rate limited: %s", e.Message)
}
