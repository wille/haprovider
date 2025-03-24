package internal

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type RPCRequest struct {
	Version string `json:"jsonrpc"`
	ID      any    `json:"id,omitempty"`
	Method  string `json:"method,omitempty"`
	Params  any    `json:"params,omitempty"`
}

func SerializeRPCRequest(req *RPCRequest) []byte {
	b, _ := json.Marshal(req)
	return b
}

func SerializeRPCResponse(req *RPCResponse) []byte {
	b, _ := json.Marshal(req)
	return b
}

func DecodeRPCRequest(b []byte) (*RPCRequest, error) {
	var req RPCRequest
	err := json.Unmarshal(b, &req)
	if err != nil {
		return nil, err
	}

	return &req, nil
}

func DecodeRPCResponse(b []byte) (*RPCResponse, error) {
	var res RPCResponse
	err := json.Unmarshal(b, &res)
	if err != nil {
		return nil, err
	}

	return &res, nil
}

func NewRPCRequest(id string, method string, params any) *RPCRequest {
	return &RPCRequest{
		Version: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}
}

type RPCResponse struct {
	Version string `json:"jsonrpc"`

	// ID might be a string or number
	ID     any    `json:"id,omitempty"`
	Result any    `json:"result"`
	Method string `json:"method,omitempty"`
	Params any    `json:"params,omitempty"`
	Error  any    `json:"error,omitempty"`
}

func (r *RPCResponse) IsError() bool {
	return r.Error != nil
}

func (r *RPCResponse) GetError() (int, error) {
	err, ok := r.Error.(map[string]any)
	if !ok {
		return 0, fmt.Errorf("unable to parse error: %v", r.Error)
	}

	errorCode := err["code"].(float64)

	return int(errorCode), fmt.Errorf("%d %s", int(errorCode), err["message"])
}

func HexToInt(s string) (int, error) {
	s = strings.TrimPrefix(s, "0x")
	i, err := strconv.ParseInt(s, 16, 64)
	return int(i), err
}

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
