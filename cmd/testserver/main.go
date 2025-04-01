package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/gorilla/websocket"
	"github.com/wille/haprovider/internal"
	"github.com/wille/haprovider/internal/rpc"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func handleRequest(body []byte) (*rpc.Response, error) {
	var req rpc.Request
	err := json.Unmarshal(body, &req)
	if err != nil {
		return nil, err
	}

	resp := &rpc.Response{
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
	case "eth_blockNumber":
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

		w.Write(rpc.SerializeResponse(resp))

		return
	}

	// Upgrade initial HTTP connection to WebSocket
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer ws.Close()

	client := internal.NewClient(ws)

	fmt.Println("Client connected")

	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-client.Read():
			rs, err := handleRequest(msg)
			if err != nil {
				fmt.Println("Error handling request:", err)
				return
			}

			client.Write(rpc.SerializeResponse(rs))
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
