package ws

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type Client struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	Conn   *websocket.Conn

	send chan []byte
	recv chan []byte

	close chan []byte

	// mu guards closed, pingStart and Latency, which are touched by the
	// readPump and writePump goroutines and by concurrent Close callers.
	mu     sync.Mutex
	closed bool

	pingcnt int

	// When we sent off a ping
	pingStart time.Time

	// Current latency of the connection
	Latency time.Duration
}

func NewClient(conn *websocket.Conn) *Client {
	ctx, cancel := context.WithCancelCause(context.Background())
	client := &Client{
		ctx:    ctx,
		cancel: cancel,
		Conn:   conn,
		send:   make(chan []byte, 32),
		recv:   make(chan []byte, 32),
		close:  make(chan []byte),
	}

	go client.readPump()
	go client.writePump()

	return client
}

func (c *Client) Read() <-chan []byte {
	return c.recv
}

func (c *Client) Write(data []byte) {
	select {
	case <-c.ctx.Done():
		slog.Debug("write to closed channel", "ip", c.Conn.RemoteAddr())
		return
	default:
	}

	c.send <- data
}

// Close will gracefully close the connection with a close message. It is safe
// to call concurrently and more than once; only the first call sends a close
// frame, and the send never blocks if the writePump has already exited.
func (c *Client) Close(closeCode int, cause error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()

	msg := websocket.FormatCloseMessage(closeCode, "")
	if cause != nil {
		msg = websocket.FormatCloseMessage(closeCode, cause.Error())
	}

	select {
	case c.close <- msg:
	case <-c.ctx.Done():
	}

	c.cancel(cause)
}

// destroy will force destroy the connection
func (c *Client) destroy(cause error) {
	c.cancel(cause)
}

func (c *Client) readPump() {
	defer func() { _ = c.Conn.Close() }()

	_ = c.Conn.SetReadDeadline(time.Now().Add(pongWait))
	c.Conn.SetPongHandler(func(string) error {
		_ = c.Conn.SetReadDeadline(time.Now().Add(pongWait))
		c.mu.Lock()
		c.Latency = time.Since(c.pingStart)
		c.mu.Unlock()
		return nil
	})

	for {
		_, message, err := c.Conn.ReadMessage()

		if err != nil {
			c.destroy(err)
			return
		}

		c.recv <- message
	}
}

func (c *Client) writePump() {
	tick := time.NewTicker(pingPeriod)
	defer tick.Stop()
	defer func() { _ = c.Conn.Close() }()

	for {
		select {
		case <-c.ctx.Done():
			return
		case data := <-c.close:
			_ = c.Conn.WriteMessage(websocket.CloseMessage, data)
			return
		case <-tick.C:
			_ = c.Conn.SetWriteDeadline(time.Now().Add(writeWait))

			pingData := []byte("haprovider/" + strconv.Itoa(c.pingcnt))
			c.pingcnt++
			c.mu.Lock()
			c.pingStart = time.Now()
			c.mu.Unlock()

			err := c.Conn.WriteMessage(websocket.PingMessage, pingData)
			if err != nil {
				c.destroy(fmt.Errorf("write: %s", err))
				return
			}

		case message, ok := <-c.send:
			_ = c.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				c.destroy(fmt.Errorf("write: channel closed"))
				return
			}

			err := c.Conn.WriteMessage(websocket.TextMessage, message)
			if err != nil {
				c.destroy(fmt.Errorf("write: %s", err))
				return
			}

			// Flush any pending queue of messages
			for i := 0; i < len(c.send); i++ {
				err := c.Conn.WriteMessage(websocket.TextMessage, <-c.send)
				if err != nil {
					c.destroy(fmt.Errorf("write: %s", err))
					return
				}
			}
		}
	}
}
