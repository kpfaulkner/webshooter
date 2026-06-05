package main

import (
	"encoding/json"
	"time"
)

// inputMsg carries a parsed client input tagged with its sender.
type inputMsg struct {
	id string
	in input
}

// Hub is the single owner of one room's game state. All mutation happens inside
// run, driven by channels, so the game needs no locks.
type Hub struct {
	clients    map[*Client]bool
	register   chan *Client
	unregister chan *Client
	inputs     chan inputMsg
	game       *game

	// Room identity within the Manager. password is this room's key; pending
	// counts joins handed out by Manager.acquire but not yet registered, which
	// guards the room from being reaped mid-join. Both are owned by the Manager
	// (accessed under its lock). manager is nil in unit tests that drive the game
	// directly without a room registry.
	manager  *Manager
	password string
	pending  int
}

func newHub(levels [][]rect, names []string) *Hub {
	return &Hub{
		clients:    make(map[*Client]bool),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		inputs:     make(chan inputMsg, 256),
		game:       newGame(levels, names),
	}
}

func (h *Hub) run() {
	ticker := time.NewTicker(time.Second / tickRate)
	defer ticker.Stop()

	last := time.Now()
	for {
		select {
		case c := <-h.register:
			h.clients[c] = true
			h.game.addPlayer(c.id)
			h.sendInit(c)
			h.manager.registered(h)

		case c := <-h.unregister:
			if _, ok := h.clients[c]; ok {
				delete(h.clients, c)
				close(c.send)
				h.game.removePlayer(c.id)
			}
			// Once the room is empty (and no join is in flight), retire it so
			// idle rooms don't pile up. reapIfEmpty deletes it from the registry.
			if h.manager.reapIfEmpty(h) {
				return
			}

		case msg := <-h.inputs:
			h.game.applyInput(msg.id, msg.in)

		case <-ticker.C:
			now := time.Now()
			dt := now.Sub(last).Seconds()
			last = now
			h.game.update(dt)
			if h.game.levelDirty {
				h.broadcastLevel()
				h.game.levelDirty = false
			}
			h.broadcast()
		}
	}
}

// --- outbound messages ---

type initMsg struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	// World and movement constants so the client predicts with identical math.
	Width      float64 `json:"width"`
	Height     float64 `json:"height"`
	Speed      float64 `json:"speed"`
	Radius     float64 `json:"radius"`
	WinScore   int     `json:"winScore"`
	Obstacles  []rect  `json:"obstacles"`
	Level      int     `json:"level"`      // 1-based index of the active level
	LevelCount int     `json:"levelCount"` // total levels in the playlist
	LevelName  string  `json:"levelName"`
}

// levelMsg is broadcast when the level changes between rounds so clients update
// their obstacle layout (used for both rendering and prediction collision).
type levelMsg struct {
	Type       string `json:"type"`
	Obstacles  []rect `json:"obstacles"`
	Level      int    `json:"level"`
	LevelCount int    `json:"levelCount"`
	LevelName  string `json:"levelName"`
}

type playerView struct {
	ID    string `json:"id"`
	Pos   vec    `json:"pos"`
	Aim   vec    `json:"aim"`
	Score int    `json:"score"`
	Alive bool   `json:"alive"`
	// Seq is the last input sequence the server processed for this player; the
	// owning client uses it to discard acknowledged inputs during reconciliation.
	Seq int `json:"seq"`
	// Bounce is the player's remaining ricochet shots (0 = none); clients use it
	// to draw a power-up aura.
	Bounce int `json:"bounce"`
}

type bulletView struct {
	ID     int  `json:"id"`
	Pos    vec  `json:"pos"`
	Bounce bool `json:"bounce"` // rendered distinctly as a ricochet round
}

// pickupView is the collectible's position, sent only while one is on the
// field (nil/omitted otherwise).
type pickupView struct {
	Pos vec `json:"pos"`
}

type stateMsg struct {
	Type    string       `json:"type"`
	Players []playerView `json:"players"`
	Bullets []bulletView `json:"bullets"`
	// Match state. Phase is "playing" or "ended". TimeLeft is the seconds left in
	// the round. While ended, Winner names the victor ("" = draw) and ResetIn
	// counts down (seconds) to the next round.
	Phase    string  `json:"phase"`
	Winner   string  `json:"winner"`
	TimeLeft float64 `json:"timeLeft"`
	ResetIn  float64 `json:"resetIn"`
	// Pickup is the ricochet collectible currently on the field, or omitted when
	// there is none.
	Pickup *pickupView `json:"pickup,omitempty"`
}

func (h *Hub) sendInit(c *Client) {
	g := h.game
	msg := initMsg{
		Type:       "init",
		ID:         c.id,
		Width:      worldWidth,
		Height:     worldHeight,
		Speed:      playerSpeed,
		Radius:     playerRadius,
		WinScore:   winScore,
		Obstacles:  g.obstacles,
		Level:      g.levelIndex + 1,
		LevelCount: len(g.levels),
		LevelName:  g.levelName(),
	}
	if data, err := json.Marshal(msg); err == nil {
		c.trySend(data)
	}
}

func (h *Hub) broadcastLevel() {
	g := h.game
	msg := levelMsg{
		Type:       "level",
		Obstacles:  g.obstacles,
		Level:      g.levelIndex + 1,
		LevelCount: len(g.levels),
		LevelName:  g.levelName(),
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	for c := range h.clients {
		c.trySend(data)
	}
}

func (h *Hub) broadcast() {
	// Non-nil slices so they marshal to [] rather than null (null is not
	// iterable on the client and would break rendering).
	state := stateMsg{
		Type:     "state",
		Players:  []playerView{},
		Bullets:  []bulletView{},
		Phase:    h.game.phase,
		Winner:   h.game.winnerID,
		TimeLeft: h.game.roundTimer,
		ResetIn:  h.game.resetTimer,
	}
	for _, p := range h.game.players {
		state.Players = append(state.Players, playerView{
			ID:     p.id,
			Pos:    p.pos,
			Aim:    p.aim,
			Score:  p.score,
			Alive:  p.alive,
			Seq:    p.lastSeq,
			Bounce: p.bounceShots,
		})
	}
	for _, b := range h.game.bullets {
		state.Bullets = append(state.Bullets, bulletView{ID: b.id, Pos: b.pos, Bounce: b.bounce})
	}
	if h.game.pickup != nil {
		state.Pickup = &pickupView{Pos: h.game.pickup.pos}
	}

	data, err := json.Marshal(state)
	if err != nil {
		return
	}
	for c := range h.clients {
		c.trySend(data)
	}
}
