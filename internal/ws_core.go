package internal

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
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

// Close will gracefully close the connection with a close message
func (c *Client) Close(closeCode int, cause error) {
	c.closed = true

	if cause != nil {
		c.close <- websocket.FormatCloseMessage(closeCode, cause.Error())
	} else {
		c.close <- websocket.FormatCloseMessage(closeCode, "")
	}

	c.cancel(cause)
}

// destroy will force destroy the connection
func (c *Client) destroy(cause error) {
	c.cancel(cause)
}

func (c *Client) readPump() {
	defer c.Conn.Close()

	c.Conn.SetReadDeadline(time.Now().Add(pongWait))
	c.Conn.SetPongHandler(func(string) error {
		c.Conn.SetReadDeadline(time.Now().Add(pongWait))
		c.Latency = time.Since(c.pingStart)
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
	defer c.Conn.Close()

	for {
		select {
		case <-c.ctx.Done():
			return
		case data := <-c.close:
			c.Conn.WriteMessage(websocket.CloseMessage, data)
			return
		case <-tick.C:
			c.Conn.SetWriteDeadline(time.Now().Add(writeWait))

			pingData := []byte("haprovider/" + strconv.Itoa(c.pingcnt))
			c.pingcnt++
			c.pingStart = time.Now()

			err := c.Conn.WriteMessage(websocket.PingMessage, pingData)
			if err != nil {
				c.destroy(fmt.Errorf("write: %s", err))
				return
			}

		case message, ok := <-c.send:
			c.Conn.SetWriteDeadline(time.Now().Add(writeWait))
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
