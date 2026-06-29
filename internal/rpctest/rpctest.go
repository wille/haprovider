// Package rpctest provides a mock upstream node used by chain healthcheck tests.
package rpctest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"

	"github.com/gorilla/websocket"
	"github.com/wille/haprovider/internal/rpc"
)

// RPCError, used as a method's mapped value, makes the mock return a JSON-RPC
// error response with this code and message instead of a result.
type RPCError struct {
	Code    int
	Message string
}

// NewServer returns a mock node backed by httptest.
//
// methods maps a JSON-RPC method name to the value placed in the response
// "result" field (or an RPCError to return a JSON-RPC error instead). paths maps
// a raw URL path (e.g. TRON's "/wallet/getnowblock") to a raw JSON response body
// and is matched before JSON-RPC dispatch. Unknown methods return a -32601 error.
func NewServer(methods map[string]any, paths map[string]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if body, ok := paths[r.URL.Path]; ok {
			_, _ = w.Write([]byte(body))
			return
		}

		b, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}

		batch, err := rpc.DecodeBatchRequest(b)
		if err != nil || len(batch.Requests) == 0 {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		out, err := rpc.SerializeBatchResponse(buildBatchResponse(methods, batch))
		if err != nil {
			http.Error(w, "serialize error", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(out)
	}))
}

// buildBatchResponse answers every request in a (possibly batched) request,
// preserving batch-ness so single requests get a single object response. An
// unknown method returns a -32601 error; a method mapped to an RPCError returns
// that JSON-RPC error.
func buildBatchResponse(methods map[string]any, batch *rpc.BatchRequest) *rpc.BatchResponse {
	responses := make([]*rpc.Response, 0, len(batch.Requests))
	for _, req := range batch.Requests {
		res := &rpc.Response{Version: "2.0", ID: req.ID}

		result, ok := methods[req.Method]
		if !ok {
			res.Error = rpc.NewError(-32601, "method not found: "+req.Method)
		} else if e, isErr := result.(RPCError); isErr {
			res.Error = rpc.NewError(e.Code, e.Message)
		} else if b, err := json.Marshal(result); err != nil {
			res.Error = rpc.NewError(-32603, err.Error())
		} else {
			res.Result = b
		}

		responses = append(responses, res)
	}

	return &rpc.BatchResponse{Responses: responses, IsBatch: batch.IsBatch}
}

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// NewWSServer returns a mock upstream that speaks JSON-RPC over WebSocket, using
// the same methods map semantics as NewServer.
//
// statusOnUpgrade lets a test simulate a non-101 handshake response: when it's
// not http.StatusSwitchingProtocols the handler writes that status (with a
// Retry-After header) WITHOUT upgrading, which is exactly what DialProvider
// inspects to detect a 429 on the handshake.
func NewWSServer(methods map[string]any, statusOnUpgrade int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if statusOnUpgrade != http.StatusSwitchingProtocols {
			w.Header().Set("Retry-After", "5")
			w.WriteHeader(statusOnUpgrade)
			return
		}

		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		// The proxy splits batches and sends one request per message, but decode
		// as a batch so this mock handles both shapes. gorilla auto-replies to
		// the proxy's pings during ReadMessage, so no ping handling is needed.
		for {
			_, message, err := conn.ReadMessage()
			if err != nil {
				return
			}

			batch, err := rpc.DecodeBatchRequest(message)
			if err != nil || len(batch.Requests) == 0 {
				continue
			}

			out, err := rpc.SerializeBatchResponse(buildBatchResponse(methods, batch))
			if err != nil {
				return
			}
			if err := conn.WriteMessage(websocket.TextMessage, out); err != nil {
				return
			}
		}
	}))
}
