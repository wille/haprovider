package rpc

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type Response struct {
	Version string `json:"jsonrpc"`

	// ID is a string, number, or null (validated by the ID type).
	ID ID `json:"id,omitempty"`

	// Result is the raw JSON of a success response, decoded on demand with DecodeResult[T].
	// Keeping it raw avoids a decode→re-encode round-trip and preserves number
	// precision (a JSON number decoded into `any` becomes a lossy float64).
	Result json.RawMessage `json:"result"`

	Method string `json:"method"`

	// Params is raw JSON. Subscription notifications carry their (often large)
	// payload here; keeping it raw lets the proxy forward it verbatim without a
	// decode→re-encode round-trip.
	Params json.RawMessage `json:"params"`

	// Error is the raw JSON error object, present only on error responses. Kept
	// raw so any shape (including non-spec ones) and the `data` field are
	// forwarded to the client verbatim; GetError decodes it leniently.
	Error json.RawMessage `json:"error,omitempty"`
}

func (r Response) MarshalJSON() ([]byte, error) {
	// JSON-RPC notification: {"jsonrpc":"2.0","method":"...","params":...}
	if r.Method != "" {
		type notification struct {
			Version string          `json:"jsonrpc"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params,omitempty"`
		}
		return json.Marshal(notification{
			Version: r.Version,
			Method:  r.Method,
			Params:  r.Params,
		})
	}

	// Error response: {"jsonrpc":"2.0","id":...,"error":...}
	if len(r.Error) > 0 {
		type errorResponse struct {
			Version string          `json:"jsonrpc"`
			ID      ID              `json:"id,omitempty"`
			Error   json.RawMessage `json:"error"`
		}
		return json.Marshal(errorResponse{
			Version: r.Version,
			ID:      r.ID,
			Error:   r.Error,
		})
	}

	// Success response: {"jsonrpc":"2.0","id":...,"result":...}
	// Note: result must be included even when it's null.
	type successResponse struct {
		Version string          `json:"jsonrpc"`
		ID      ID              `json:"id,omitempty"`
		Result  json.RawMessage `json:"result"`
	}
	return json.Marshal(successResponse{
		Version: r.Version,
		ID:      r.ID,
		Result:  r.Result,
	})
}

func (r *Response) GetID() string {
	return r.ID.String()
}

func (r *Response) IsError() bool {
	return len(r.Error) > 0 && string(r.Error) != "null"
}

// GetError leniently decodes the error object, returning its code and a
// formatted error. A non-spec error shape (e.g. a bare string) yields code 0
// and the raw text rather than failing, so the proxy can still forward it.
func (r *Response) GetError() (int, error) {
	var e RPCError
	if err := json.Unmarshal(r.Error, &e); err != nil {
		return 0, fmt.Errorf("rpc error: %s", FormatRawBody(string(r.Error)))
	}

	return e.Code, fmt.Errorf("rpc %d: %s", e.Code, e.Message)
}

func SerializeResponse(res *Response) ([]byte, error) {
	return json.Marshal(res)
}

func DecodeResponse(b []byte) (*Response, error) {
	var res Response
	err := json.Unmarshal(b, &res)
	if err != nil {
		return nil, err
	}

	return &res, nil
}

// DecodeResult decodes a successful response's result into T. It returns an
// error if the response is missing or carries a JSON-RPC error, so a caller
// never receives a zero-valued T that silently masks an upstream failure.
func DecodeResult[T any](r *Response) (*T, error) {
	if r == nil {
		return nil, fmt.Errorf("response is nil")
	}
	if r.IsError() {
		_, err := r.GetError()
		return nil, err
	}

	var t T
	if err := json.Unmarshal(r.Result, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func WriteResponse(w http.ResponseWriter, res *Response) error {
	b, err := SerializeResponse(res)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}
