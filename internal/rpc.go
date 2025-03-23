package internal

import (
	"encoding/json"
	"strconv"
	"strings"
)

type RPCRequest struct {
	Version string      `json:"jsonrpc"`
	ID      interface{} `json:"id,omitempty"`
	Method  string      `json:"method,omitempty"`
	Params  interface{} `json:"params,omitempty"`
}

func SerializeRPCRequest(req *RPCRequest) []byte {
	b, _ := json.Marshal(req)
	return b
}

func SerializeRPCResponse(req *RPCResponse) []byte {
	b, _ := json.Marshal(req)
	return b
}

func NewRPCRequest(method string, params interface{}) *RPCRequest {
	return &RPCRequest{
		Version: "2.0",
		ID:      "1", // tood
		Method:  method,
		Params:  params,
	}
}

type RPCResponse struct {
	Version string `json:"jsonrpc"`

	// ID might be a string or number
	ID     interface{} `json:"id,omitempty"`
	Result interface{} `json:"result"`
	Method string      `json:"method,omitempty"`
	Params interface{} `json:"params,omitempty"`
	Error  interface{} `json:"error,omitempty"`
}

func HexToInt(s string) (int, error) {
	s = strings.TrimPrefix(s, "0x")
	i, err := strconv.ParseInt(s, 16, 64)
	return int(i), err
}

func GetClientID(zz interface{}) (string, string) {
	var id string

	switch zz.(type) {
	case string:
		id = zz.(string)
	case float64:
		id = strconv.Itoa(int(zz.(float64)))
	}

	if zz == nil {
		return "", ""
	}

	k := strings.Split(id, ":")
	if len(k) > 1 {
		return k[0], k[1]
	}
	return k[0], ""
}

func ValidateRequestBody() {

}

func ValidateResponseBody() {

}

// FormatRawBody formats a response body so we can include it in log output in a human readable format
func FormatRawBody(body string) string {
	if len(body) > 64 {
		body = body[:64] + "... (truncated)"
	}

	body = strings.ReplaceAll(body, "\n", " ")

	return body
}
