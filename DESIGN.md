# Webshooter — Design Notes

## Vision
A simple browser-based, multiplayer, top-down shooting game.

- Players join from a web browser — no install.
- Each player moves around a 2D top-down landscape.
- Players shoot at each other; landing a hit scores points.
- Multiplayer over the network in real time.

## Core gameplay (v1 scope)
- **Movement**: top-down, WASD / arrow keys to move; mouse to aim.
- **Shooting**: click (or space) to fire in the aim direction.
- **Scoring**: hitting another player = points. Killed player respawns.
- **Match**: free-for-all deathmatch. Optional time limit / score limit.
- **Players**: handful per match to start (e.g. 2–8).

## Architecture (proposed)
Client/server with an **authoritative server** — the server owns the true game
state to prevent cheating and resolve conflicts.

```
Browser client (HTML5 Canvas + JS)
        │  WebSocket (JSON or binary)
        ▼
Go server (github.com/kpfaulkner/webshooter)
  - accepts WebSocket connections
  - maintains authoritative world state
  - runs the game loop / tick
  - broadcasts state to all clients
```

### Server (Go)
- Serve static client files (HTML/JS) over HTTP.
- WebSocket endpoint for game traffic.
- Fixed-tick game loop (e.g. 20–30 ticks/sec):
  - apply queued player inputs
  - update positions, resolve bullet/player collisions
  - update scores, handle respawns
  - broadcast snapshot to all clients
- One "room"/world to start; multiple rooms later.

### Client (browser)
- HTML5 Canvas for rendering the top-down view.
- Capture input (keyboard + mouse), send to server.
- Render the latest server snapshot (with interpolation for smoothness later).

### Network protocol
- Client → server: player inputs (move direction, aim, fire).
- Server → client: world snapshot (player positions, bullets, scores).
- Start with JSON for simplicity; revisit binary if bandwidth matters.

## Open questions / decisions to make
- Map: open arena vs. obstacles/walls? Fixed size vs. scrolling.
- Authoritative vs. client-predicted movement (prediction = smoother, more complex).
- Snapshot rate vs. tick rate; interpolation/lag compensation.
- Bullets: hitscan (instant) vs. travelling projectiles.
- Matchmaking: single shared room vs. lobbies/rooms.
- Persistence: are scores/accounts saved, or per-session only?

## Tech choices (tentative)
- **Backend**: Go. WebSockets via `nhooyr.io/websocket` or `gorilla/websocket`.
- **Frontend**: vanilla JS + Canvas to keep it simple (no framework needed).
- **Transport**: WebSocket (TCP). WebRTC/UDP only if latency demands it later.

## Out of scope for v1
- Accounts / login, persistent stats, leaderboards.
- Multiple weapons, power-ups, teams.
- Mobile controls.
- Anti-cheat beyond server authority.
