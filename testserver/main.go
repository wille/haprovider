package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/gorilla/websocket"
	"github.com/wille/haprovider/internal"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func handleRequest(body []byte) (*internal.RPCResponse, error) {
	var req internal.RPCRequest
	err := json.Unmarshal(body, &req)
	if err != nil {
		return nil, err
	}

	resp := &internal.RPCResponse{
		Version: "2.0",
		ID:      req.ID,
	}

	switch req.Method {
	case "web3_clientVersion":
		resp.Result = "testserver"
		return resp, nil
	case "eth_chainId":
		resp.Result = "0x1"
		return resp, nil
	case "ha_ratelimit":
		resp.Error = map[string]any{
			"code":    -32005,
			"message": "Too many requests",
		}
		return resp, nil
	}

	return nil, fmt.Errorf("unknown method: %s", req.Method)
}

// Handle WebSocket connections
func handleConnections(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Upgrade") != "websocket" {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			log.Println("Error reading body", err)
			http.Error(w, "Error reading body", http.StatusInternalServerError)
			return
		}

		resp, err := handleRequest(body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		w.Write(internal.SerializeRPCResponse(resp))

		return
	}

	// Upgrade initial HTTP connection to WebSocket
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer ws.Close()

	ctx, cancel := context.WithCancelCause(r.Context())
	client := internal.NewClient(ctx, cancel, ws)

	fmt.Println("Client connected")

	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-client.Read():
			rs, err := handleRequest(msg)
			if err != nil {
				fmt.Println("Error handling request:", err)
				continue
			}

			client.Write(internal.SerializeRPCResponse(rs))
		}
	}
}

func CreateRPCServer() *http.Server {
	server := &http.Server{
		Addr:    ":6969",
		Handler: http.HandlerFunc(handleConnections),
	}

	return server
}

func main() {
	server := CreateRPCServer()
	fmt.Println("WebSocket server running on ", server.Addr)
	err := server.ListenAndServe()
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}
