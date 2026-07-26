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

// JSON-RPC error codes that haprovider itself puts in responses it generates for
// clients (as opposed to codes it receives from providers, which each chain
// interprets in its own HandleError).
const (
	// CodeParseError ("Parse error") is returned when the client's request body is
	// not valid JSON / JSON-RPC.
	CodeParseError = -32700

	// CodeInvalidRequest ("Invalid Request") is returned when the request is
	// malformed or unacceptable (wrong content type, too large, batch too large).
	CodeInvalidRequest = -32600

	// CodeResourceUnavailable ("Resource Unavailable") is returned when a chain
	// does not allow a client to call a method.
	CodeResourceUnavailable = -32002

	// CodeMethodNotFound is the JSON-RPC standard "Method not found", returned when
	// no provider supports the requested method.
	CodeMethodNotFound = -32601

	// CodeInternalError ("Internal error") is returned for a failure inside the
	// proxy itself (reading the body, serializing a response, an unclassified
	// upstream failure).
	CodeInternalError = -32603

	// CodeNoProvidersAvailable is a haprovider-specific code (outside the standard
	// ranges) returned when no provider can currently serve the request.
	CodeNoProvidersAvailable = -39200
)

var ErrorResponseNoProvidersAvailable = Response{
	Version: "2.0",
	ID:      nil,
	Error:   NewError(CodeNoProvidersAvailable, "no providers available"),
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

// ErrorTryNextProvider signals that a provider is healthy but cannot serve this
// particular request (e.g. it doesn't have the requested Solana slot/block). The
// failover loop tries the next provider without marking this one unhealthy and
// without caching anything; if no provider can serve the request, the upstream
// error is returned to the client.
type ErrorTryNextProvider struct {
	Message string
}

var _ error = (*ErrorTryNextProvider)(nil)

func (e *ErrorTryNextProvider) Error() string {
	return fmt.Sprintf("try next provider: %s", e.Message)
}
