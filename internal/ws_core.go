package internal

import (
	"context"
	"fmt"
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

	pingTimer *time.Ticker

	// When we sent off a ping
	pingStart time.Time

	// Current latency of the connection
	Latency time.Duration
}

func NewClient(ctx context.Context, cancel context.CancelCauseFunc, conn *websocket.Conn) *Client {
	ctx, cancel = context.WithCancelCause(context.Background())
	client := &Client{
		ctx:       ctx,
		cancel:    cancel,
		Conn:      conn,
		send:      make(chan []byte, 256),
		recv:      make(chan []byte, 256),
		close:     make(chan []byte, 1),
		pingTimer: time.NewTicker(pingPeriod),
	}

	go client.readPump()
	go client.writePump()

	return client
}

func (c *Client) Read() <-chan []byte {
	return c.recv
}

func (c *Client) Write(data []byte) {
	// Make sure we're never writing to a closed channel
	select {
	case <-c.ctx.Done():
		return
	default:
	}

	c.send <- data
}

// Close will gracefully close the connection with a close message
func (c *Client) Close(closeCode int, text error) {
	if c.closed {
		return
	}
	c.closed = true

	if text != nil {
		c.close <- websocket.FormatCloseMessage(closeCode, text.Error())
	} else {
		c.close <- websocket.FormatCloseMessage(closeCode, "")
	}

	// Remember that <-close will ws.Close()
	c.stop(text)

	// c.pingTimer.Stop()

	// close(c.close)

}

// stop will force stop the connection
func (c *Client) stop(cause error) {
	c.cancel(cause)
}

func (c *Client) readPump() {
	defer close(c.recv)

	c.Conn.SetReadDeadline(time.Now().Add(pongWait))
	c.Conn.SetPongHandler(func(string) error {
		c.Conn.SetReadDeadline(time.Now().Add(pongWait))
		c.Latency = time.Since(c.pingStart)
		return nil
	})

	for {
		_, message, err := c.Conn.ReadMessage()

		if err != nil {
			c.stop(err)
			return
		}

		c.recv <- message
	}
}

func (c *Client) writePump() {
	defer close(c.close)
	defer c.pingTimer.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case data := <-c.close:
			c.Conn.WriteMessage(websocket.CloseMessage, data)
			c.Conn.Close()
			return
		case <-c.pingTimer.C:
			c.Conn.SetWriteDeadline(time.Now().Add(writeWait))

			pingData := []byte("haprovider/" + strconv.Itoa(c.pingcnt))
			c.pingcnt++
			c.pingStart = time.Now()

			err := c.Conn.WriteMessage(websocket.PingMessage, pingData)
			if err != nil {
				c.stop(fmt.Errorf("write: %s", err))
				return
			}

		case message, ok := <-c.send:
			c.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				c.stop(fmt.Errorf("write: channel closed"))
				return
			}

			err := c.Conn.WriteMessage(websocket.TextMessage, message)
			if err != nil {
				c.stop(fmt.Errorf("write: %s", err))
				return
			}

			// Flush any pending queue of messages
			for i := 0; i < len(c.send); i++ {
				err := c.Conn.WriteMessage(websocket.TextMessage, <-c.send)
				if err != nil {
					c.stop(fmt.Errorf("write: %s", err))
					return
				}
			}
		}
	}
}
