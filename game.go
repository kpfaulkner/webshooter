package main

import (
	"math"
	"math/rand"
)

// World and entity tuning constants. All distances are in pixels, speeds in
// pixels/second, times in seconds.
const (
	tickRate = 30

	worldWidth  = 1024
	worldHeight = 768

	playerRadius = 14.0
	playerSpeed  = 220.0

	bulletRadius = 4.0
	bulletSpeed  = 560.0
	bulletLife   = 1.5
	fireCooldown = 0.25

	respawnDelay = 2.0

	// maxInputDt clamps the client-supplied frame time so a malicious or lagging
	// client can't teleport by claiming a huge dt.
	maxInputDt = 0.05

	// winScore is the number of frags that ends the match; roundDuration is the
	// time limit after which the highest score wins; matchResetDelay is how long
	// the result is shown before a fresh round starts.
	winScore        = 10
	roundDuration   = 120.0
	matchResetDelay = 5.0
)

// Match phases.
const (
	phasePlaying = "playing"
	phaseEnded   = "ended"
)

type vec struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// rect is an axis-aligned obstacle. Players collide with it and bullets are
// blocked by it. The active level's rects are sent to clients in init and again
// (via a "level" message) whenever the level changes.
type rect struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// defaultLevel is the built-in fallback layout, used when no level files load.
// Keep entries clear of the world edges so push-out never shoves a player out
// of bounds.
var defaultLevel = []rect{
	{X: 250, Y: 140, W: 130, H: 40},
	{X: 640, Y: 180, W: 40, H: 220},
	{X: 420, Y: 430, W: 220, H: 40},
	{X: 150, Y: 500, W: 40, H: 150},
	{X: 820, Y: 540, W: 150, H: 40},
}

// input is a single timestamped control command from a client. Seq lets the
// client match server acknowledgements to its pending inputs (for prediction
// reconciliation); Dt is the frame time the client integrated this command
// over, so the server can reproduce the same movement.
type input struct {
	Seq         int     `json:"seq"`
	Forward     bool    `json:"forward"`     // toward the aim direction
	Back        bool    `json:"back"`        // away from the aim direction
	StrafeLeft  bool    `json:"strafeLeft"`  // perpendicular, left of aim
	StrafeRight bool    `json:"strafeRight"` // perpendicular, right of aim
	AimX        float64 `json:"aimX"`
	AimY        float64 `json:"aimY"`
	Fire        bool    `json:"fire"`
	Dt          float64 `json:"dt"`
}

type player struct {
	id    string
	pos   vec
	aim   vec // normalized facing direction
	score int
	alive bool

	lastSeq      int     // sequence number of the last input we processed
	fireTimer    float64 // seconds until next shot allowed
	respawnTimer float64 // seconds until respawn while dead
	deathPos     vec     // where we last died, so respawn can stay clear of it
}

type bullet struct {
	id    int
	owner string
	pos   vec
	vel   vec
	life  float64
}

// game holds the authoritative world state. It is owned by the hub goroutine,
// so no locking is required — all access happens from hub.run.
type game struct {
	players    map[string]*player
	bullets    []*bullet
	nextBullet int

	phase      string  // phasePlaying or phaseEnded
	winnerID   string  // set while phaseEnded ("" means a draw)
	roundTimer float64 // seconds left in the current round
	resetTimer float64 // seconds left until the next round starts

	levels     [][]rect // the level playlist, cycled through on each win
	levelNames []string // display names, parallel to levels
	levelIndex int      // index of the active level
	obstacles  []rect   // == levels[levelIndex]; the active layout
	levelDirty bool     // set when the level changes so the hub rebroadcasts it
}

// newGame builds a game over the given level playlist. Empty levels fall back
// to the built-in defaultLevel.
func newGame(levels [][]rect, names []string) *game {
	if len(levels) == 0 {
		levels = [][]rect{defaultLevel}
		names = []string{"default"}
	}
	return &game{
		players:    make(map[string]*player),
		phase:      phasePlaying,
		roundTimer: roundDuration,
		levels:     levels,
		levelNames: names,
		levelIndex: 0,
		obstacles:  levels[0],
	}
}

func (g *game) levelName() string {
	if g.levelIndex < len(g.levelNames) {
		return g.levelNames[g.levelIndex]
	}
	return ""
}

func (g *game) addPlayer(id string) {
	g.players[id] = &player{
		id:    id,
		pos:   g.randomSpawn(),
		aim:   vec{X: 1, Y: 0},
		alive: true,
	}
}

func (g *game) removePlayer(id string) {
	delete(g.players, id)
}

// applyInput integrates a single client command immediately on arrival. Doing
// this per-input (rather than once per tick) lets the client reproduce the
// exact same movement during prediction/reconciliation. Firing cadence stays
// on server time (the fireTimer is decremented in update) so the cooldown can't
// be cheated via the client-supplied dt.
func (g *game) applyInput(id string, in input) {
	p, ok := g.players[id]
	if !ok {
		return
	}
	p.lastSeq = in.Seq
	// Freeze controls between rounds, but keep acking inputs so the client's
	// reconciliation stays in sync.
	if !p.alive || g.phase != phasePlaying {
		return
	}

	dt := clamp(in.Dt, 0, maxInputDt)

	// Movement is relative to the aim direction: forward goes toward the cursor,
	// strafing moves perpendicular to it. The client predicts with the exact
	// same math (see integrate in game.js) so this must stay in sync.
	fwd := normalize(vec{X: in.AimX, Y: in.AimY})
	right := vec{X: -fwd.Y, Y: fwd.X} // 90° clockwise of forward in screen coords

	var dir vec
	if in.Forward {
		dir.X += fwd.X
		dir.Y += fwd.Y
	}
	if in.Back {
		dir.X -= fwd.X
		dir.Y -= fwd.Y
	}
	if in.StrafeRight {
		dir.X += right.X
		dir.Y += right.Y
	}
	if in.StrafeLeft {
		dir.X -= right.X
		dir.Y -= right.Y
	}
	dir = normalize(dir)
	p.pos.X += dir.X * playerSpeed * dt
	p.pos.Y += dir.Y * playerSpeed * dt
	p.pos.X = clamp(p.pos.X, playerRadius, worldWidth-playerRadius)
	p.pos.Y = clamp(p.pos.Y, playerRadius, worldHeight-playerRadius)
	p.pos = g.resolveObstacles(p.pos, playerRadius)

	// Update facing toward the cursor (keep previous facing if cursor is on us).
	if fwd != (vec{}) {
		p.aim = fwd
	}

	// Firing, gated by a per-player cooldown enforced on server time.
	if in.Fire && p.fireTimer <= 0 {
		g.spawnBullet(p)
		p.fireTimer = fireCooldown
	}
}

// update advances time-based world state by dt seconds: respawns, fire
// cooldowns, and bullet motion. Player movement is handled in applyInput.
func (g *game) update(dt float64) {
	// Between rounds the world is frozen; just count down to the next round.
	if g.phase == phaseEnded {
		g.resetTimer -= dt
		if g.resetTimer <= 0 {
			g.resetMatch()
		}
		return
	}

	// Time limit: when it expires, the highest score wins.
	g.roundTimer -= dt
	if g.roundTimer <= 0 {
		g.roundTimer = 0
		g.endMatch(g.leader())
		return
	}

	for _, p := range g.players {
		if !p.alive {
			p.respawnTimer -= dt
			if p.respawnTimer <= 0 {
				p.alive = true
				p.pos = g.spawnAwayFrom(p.deathPos, p.id)
			}
			continue
		}
		if p.fireTimer > 0 {
			p.fireTimer -= dt
		}
	}
	g.updateBullets(dt)
}

// endMatch freezes play and starts the inter-round countdown.
func (g *game) endMatch(winnerID string) {
	g.phase = phaseEnded
	g.winnerID = winnerID
	g.resetTimer = matchResetDelay
	g.bullets = nil
}

// resetMatch starts a fresh round on the next level (wrapping to the first
// after the last): scores cleared, everyone respawned.
func (g *game) resetMatch() {
	g.advanceLevel()
	for _, p := range g.players {
		p.score = 0
		p.alive = true
		p.pos = g.randomSpawn()
		p.fireTimer = 0
		p.respawnTimer = 0
	}
	g.bullets = nil
	g.winnerID = ""
	g.roundTimer = roundDuration
	g.phase = phasePlaying
}

// advanceLevel moves to the next level in the playlist, cycling back to the
// first, and flags the change so the hub rebroadcasts the new layout.
func (g *game) advanceLevel() {
	if len(g.levels) == 0 {
		return
	}
	g.levelIndex = (g.levelIndex + 1) % len(g.levels)
	g.obstacles = g.levels[g.levelIndex]
	g.levelDirty = true
}

// leader returns the id of the highest-scoring player, or "" if the top score
// is tied (a draw).
func (g *game) leader() string {
	best := -1
	winner := ""
	tie := false
	for _, p := range g.players {
		switch {
		case p.score > best:
			best, winner, tie = p.score, p.id, false
		case p.score == best:
			tie = true
		}
	}
	if tie {
		return ""
	}
	return winner
}

func (g *game) spawnBullet(p *player) {
	g.nextBullet++
	g.bullets = append(g.bullets, &bullet{
		id:    g.nextBullet,
		owner: p.id,
		pos:   vec{X: p.pos.X + p.aim.X*playerRadius, Y: p.pos.Y + p.aim.Y*playerRadius},
		vel:   vec{X: p.aim.X * bulletSpeed, Y: p.aim.Y * bulletSpeed},
		life:  bulletLife,
	})
}

func (g *game) updateBullets(dt float64) {
	kept := g.bullets[:0]
	for _, b := range g.bullets {
		b.pos.X += b.vel.X * dt
		b.pos.Y += b.vel.Y * dt
		b.life -= dt

		if b.life <= 0 || b.pos.X < 0 || b.pos.X > worldWidth || b.pos.Y < 0 || b.pos.Y > worldHeight {
			continue
		}

		if g.hitsObstacle(b.pos, bulletRadius) {
			continue // wall blocks the shot
		}

		if g.checkHit(b) {
			continue // bullet consumed by the hit
		}
		kept = append(kept, b)
	}
	g.bullets = kept
}

// checkHit returns true if the bullet struck a player (and applies the score
// and death), in which case the bullet should be removed.
func (g *game) checkHit(b *bullet) bool {
	for _, p := range g.players {
		if !p.alive || p.id == b.owner {
			continue
		}
		if dist(p.pos, b.pos) <= playerRadius+bulletRadius {
			p.alive = false
			p.respawnTimer = respawnDelay
			p.deathPos = p.pos
			if shooter, ok := g.players[b.owner]; ok {
				shooter.score++
				if shooter.score >= winScore {
					g.endMatch(shooter.id)
				}
			}
			return true
		}
	}
	return false
}

func (g *game) randomSpawn() vec {
	// Retry a handful of times to find a spot clear of obstacles; fall back to
	// the last candidate if the level is unusually crowded.
	var v vec
	for i := 0; i < 50; i++ {
		v = vec{
			X: playerRadius + rand.Float64()*(worldWidth-2*playerRadius),
			Y: playerRadius + rand.Float64()*(worldHeight-2*playerRadius),
		}
		if !g.hitsObstacle(v, playerRadius) {
			break
		}
	}
	return v
}

// spawnAwayFrom returns an obstacle-clear spawn point that is comfortably far
// from both avoid (where the player died) and any living enemy, so you neither
// respawn on the spot nor right next to someone who can immediately shoot you.
// It returns the first candidate clearing both thresholds, falling back to the
// best-scoring one sampled if the map is too cramped to satisfy them.
func (g *game) spawnAwayFrom(avoid vec, selfID string) vec {
	const (
		minDeathDist = 320.0
		minEnemyDist = 220.0
		samples      = 40
	)
	var best vec
	bestScore := -1.0
	for i := 0; i < samples; i++ {
		c := g.randomSpawn()
		dDeath := dist(c, avoid)
		dEnemy := g.nearestEnemyDist(c, selfID)
		if dDeath >= minDeathDist && dEnemy >= minEnemyDist {
			return c
		}
		// Keeping clear of enemies matters most; the death spot is secondary.
		score := math.Min(dEnemy, minEnemyDist) + 0.5*math.Min(dDeath, minDeathDist)
		if score > bestScore {
			best, bestScore = c, score
		}
	}
	return best
}

// nearestEnemyDist is the distance from pos to the closest living player other
// than selfID, or +Inf if there are none.
func (g *game) nearestEnemyDist(pos vec, selfID string) float64 {
	nearest := math.Inf(1)
	for _, p := range g.players {
		if p.id == selfID || !p.alive {
			continue
		}
		if d := dist(pos, p.pos); d < nearest {
			nearest = d
		}
	}
	return nearest
}

// resolveObstacles pushes a circle (center pos, given radius) out of any
// obstacle it overlaps, so it slides along walls rather than passing through.
// Replicated verbatim on the client for prediction — keep the two in sync.
func (g *game) resolveObstacles(pos vec, radius float64) vec {
	for _, o := range g.obstacles {
		pos = pushOut(pos, radius, o)
	}
	return pos
}

func pushOut(pos vec, r float64, o rect) vec {
	minX, minY := o.X, o.Y
	maxX, maxY := o.X+o.W, o.Y+o.H

	// Closest point on the rect to the circle center.
	cx := clamp(pos.X, minX, maxX)
	cy := clamp(pos.Y, minY, maxY)
	dx, dy := pos.X-cx, pos.Y-cy
	d2 := dx*dx + dy*dy

	if d2 >= r*r {
		return pos // not overlapping
	}
	if d2 > 1e-9 {
		// Center outside the rect: push straight out along the contact normal.
		d := math.Sqrt(d2)
		push := r - d
		pos.X += dx / d * push
		pos.Y += dy / d * push
		return pos
	}
	// Center inside the rect: eject along the nearest edge.
	left, right := pos.X-minX, maxX-pos.X
	top, bottom := pos.Y-minY, maxY-pos.Y
	m := math.Min(math.Min(left, right), math.Min(top, bottom))
	switch m {
	case left:
		pos.X = minX - r
	case right:
		pos.X = maxX + r
	case top:
		pos.Y = minY - r
	default:
		pos.Y = maxY + r
	}
	return pos
}

// hitsObstacle reports whether a circle overlaps any obstacle.
func (g *game) hitsObstacle(pos vec, r float64) bool {
	for _, o := range g.obstacles {
		cx := clamp(pos.X, o.X, o.X+o.W)
		cy := clamp(pos.Y, o.Y, o.Y+o.H)
		dx, dy := pos.X-cx, pos.Y-cy
		if dx*dx+dy*dy < r*r {
			return true
		}
	}
	return false
}

func normalize(v vec) vec {
	mag := math.Hypot(v.X, v.Y)
	if mag == 0 {
		return vec{}
	}
	return vec{X: v.X / mag, Y: v.Y / mag}
}

func dist(a, b vec) float64 {
	return math.Hypot(a.X-b.X, a.Y-b.Y)
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
