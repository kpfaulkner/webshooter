package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	writeWait  = 10 * time.Second
	pongWait   = 60 * time.Second
	pingPeriod = (pongWait * 9) / 10
)

var nextClientID int64

// allowedOrigin is set from the ALLOWED_ORIGIN env var in main. Empty means
// allow any origin (development).
var allowedOrigin string

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     checkOrigin,
}

// checkOrigin guards WebSocket upgrades. Requests without an Origin header
// (non-browser clients) are allowed; browser requests must match
// allowedOrigin when it's configured.
func checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" || allowedOrigin == "" {
		return true
	}
	return origin == allowedOrigin
}

// Client is one browser connection: a read pump that forwards inputs to the
// hub and a write pump that drains the send channel to the socket.
type Client struct {
	hub  *Hub
	conn *websocket.Conn
	send chan []byte
	id   string
}

func serveWs(mgr *Manager, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade: %v", err)
		return
	}

	// The room is chosen by the ?password= query param: everyone using the same
	// password lands in the same game. Cap the length so a client can't spawn an
	// arbitrarily large registry key.
	password := r.URL.Query().Get("password")
	if len(password) > 64 {
		password = password[:64]
	}
	hub := mgr.acquire(password)

	id := strconv.FormatInt(atomic.AddInt64(&nextClientID, 1), 10)
	c := &Client{
		hub:  hub,
		conn: conn,
		send: make(chan []byte, 32),
		id:   id,
	}
	hub.register <- c // hub stays alive until this registers (guarded by pending)

	go c.writePump()
	go c.readPump()
}

// trySend queues data for the client, dropping it if the buffer is full so a
// slow client never blocks the hub.
func (c *Client) trySend(data []byte) {
	select {
	case c.send <- data:
	default:
	}
}

func (c *Client) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.conn.Close()
	}()

	c.conn.SetReadLimit(512)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("read: %v", err)
			}
			break
		}
		var in input
		if err := json.Unmarshal(data, &in); err != nil {
			continue
		}
		c.hub.inputs <- inputMsg{id: c.id, in: in}
	}
}

func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case data, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				// Hub closed the channel.
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}

		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
