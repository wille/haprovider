package rpc

import (
	"encoding/json"
	"fmt"
)

type Response struct {
	Version string `json:"jsonrpc"`

	// ID might be a string or number
	ID     any    `json:"id,omitempty"`
	Result any    `json:"result"`
	Method string `json:"method"`
	Params any    `json:"params"`

	// Error is only part of error responses.
	Error any `json:"error,omitempty"`
}

func (r Response) MarshalJSON() ([]byte, error) {
	// JSON-RPC notification: {"jsonrpc":"2.0","method":"...","params":...}
	if r.Method != "" {
		type notification struct {
			Version string `json:"jsonrpc"`
			Method  string `json:"method"`
			Params  any    `json:"params,omitempty"`
		}
		return json.Marshal(notification{
			Version: r.Version,
			Method:  r.Method,
			Params:  r.Params,
		})
	}

	// Error response: {"jsonrpc":"2.0","id":...,"error":...}
	if r.Error != nil {
		type errorResponse struct {
			Version string `json:"jsonrpc"`
			ID      any    `json:"id,omitempty"`
			Error   any    `json:"error"`
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
		Version string `json:"jsonrpc"`
		ID      any    `json:"id,omitempty"`
		Result  any    `json:"result"`
	}
	return json.Marshal(successResponse{
		Version: r.Version,
		ID:      r.ID,
		Result:  r.Result,
	})
}

func (r *Response) GetID() string {
	return GetRequestIDString(r.ID)
}

func (r *Response) IsError() bool {
	return r.Error != nil
}

func (r *Response) GetError() (int, error) {
	err, ok := r.Error.(map[string]any)
	if !ok {
		return 0, fmt.Errorf("unable to parse error: %v", r.Error)
	}

	errorCode, _ := err["code"].(float64)

	return int(errorCode), fmt.Errorf("%d %s", int(errorCode), err["message"])
}

func SerializeResponse(res *Response) []byte {
	b, err := json.Marshal(res)

	if err != nil {
		panic(err)
	}

	return b
}

func DecodeResponse(b []byte) (*Response, error) {
	var res Response
	err := json.Unmarshal(b, &res)
	if err != nil {
		return nil, err
	}

	return &res, nil
}
