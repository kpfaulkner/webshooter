package main

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadObstacles(t *testing.T) {
	dir := t.TempDir()

	good := filepath.Join(dir, "level.json")
	os.WriteFile(good, []byte(`[{"x":10,"y":20,"w":30,"h":40}]`), 0o644)
	obs, err := loadObstacles(good)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(obs) != 1 || obs[0] != (rect{X: 10, Y: 20, W: 30, H: 40}) {
		t.Fatalf("parsed layout wrong: %+v", obs)
	}

	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte(`not json`), 0o644)
	if _, err := loadObstacles(bad); err == nil {
		t.Fatal("expected error for malformed JSON")
	}

	if _, err := loadObstacles(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadLevels(t *testing.T) {
	dir := t.TempDir()
	// Out-of-order filenames to confirm they're sorted.
	os.WriteFile(filepath.Join(dir, "02-b.json"), []byte(`[{"x":1,"y":2,"w":3,"h":4}]`), 0o644)
	os.WriteFile(filepath.Join(dir, "01-a.json"), []byte(`[{"x":5,"y":6,"w":7,"h":8}]`), 0o644)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte(`ignored`), 0o644)

	levels, names, err := loadLevels(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(levels) != 2 {
		t.Fatalf("got %d levels, want 2", len(levels))
	}
	if names[0] != "01-a" || names[1] != "02-b" {
		t.Fatalf("levels not sorted by name: %v", names)
	}

	if _, _, err := loadLevels(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("expected error for missing dir")
	}
	if _, _, err := loadLevels(t.TempDir()); err == nil {
		t.Fatal("expected error for empty dir")
	}
}

func TestLevelRotationCycles(t *testing.T) {
	levels := [][]rect{
		{{X: 0, Y: 0, W: 1, H: 1}},
		{{X: 10, Y: 10, W: 1, H: 1}},
		{{X: 20, Y: 20, W: 1, H: 1}},
	}
	g := newGame(levels, []string{"a", "b", "c"})
	g.addPlayer("p")

	if g.levelIndex != 0 {
		t.Fatalf("start level = %d, want 0", g.levelIndex)
	}

	// Win and roll over the reset countdown three times -> 1, 2, then wrap to 0.
	want := []int{1, 2, 0}
	for i, w := range want {
		g.endMatch("p")
		g.update(matchResetDelay)
		if g.levelIndex != w {
			t.Fatalf("after win %d: levelIndex = %d, want %d", i+1, g.levelIndex, w)
		}
		if !sameLevel(g.obstacles, levels[w]) {
			t.Fatalf("after win %d: obstacles not switched to level %d", i+1, w)
		}
		if !g.levelDirty {
			t.Fatalf("after win %d: levelDirty not set", i+1)
		}
		g.levelDirty = false // hub would clear this after broadcasting
	}
}

func sameLevel(a, b []rect) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRespawnAwayFromDeath(t *testing.T) {
	g := newGame(nil, nil)
	g.addPlayer("a")
	p := g.players["a"]

	// Die at a known spot, then let the respawn timer elapse.
	p.alive = false
	p.deathPos = vec{X: 120, Y: 120}
	p.pos = p.deathPos
	p.respawnTimer = 0.01
	g.update(0.02)

	if !p.alive {
		t.Fatal("player did not respawn")
	}
	if d := dist(p.pos, p.deathPos); d < 250 {
		t.Fatalf("respawned too close to death: %.0f px from %+v", d, p.deathPos)
	}
}

func TestRespawnAwayFromEnemies(t *testing.T) {
	g := newGame(nil, nil)
	g.addPlayer("a")
	g.addPlayer("e")

	enemy := g.players["e"]
	enemy.alive = true
	enemy.pos = vec{X: 512, Y: 384} // sitting in the middle

	p := g.players["a"]
	p.alive = false
	p.deathPos = vec{X: 120, Y: 120}
	p.pos = p.deathPos
	p.respawnTimer = 0.01
	g.update(0.02)

	if !p.alive {
		t.Fatal("player did not respawn")
	}
	if d := dist(p.pos, enemy.pos); d < 180 {
		t.Fatalf("respawned too close to living enemy: %.0f px", d)
	}
}

func TestPlayerCannotEnterObstacle(t *testing.T) {
	g := newGame(nil, nil)
	o := g.obstacles[0]
	r := playerRadius

	// Start just left of the obstacle and try to walk right into it.
	g.addPlayer("a")
	p := g.players["a"]
	p.pos = vec{X: o.X - r - 1, Y: o.Y + o.H/2}

	for i := 0; i < 200; i++ { // push hard for many frames
		g.applyInput("a", input{Seq: i + 1, Forward: true, AimX: 1, AimY: 0, Dt: maxInputDt})
	}

	if g.hitsObstacle(p.pos, r) {
		t.Fatalf("player ended up overlapping obstacle at %+v", p.pos)
	}
	if p.pos.X > o.X { // never crossed into the wall's x-span at its mid-height
		t.Fatalf("player passed into obstacle: x=%v (wall starts at %v)", p.pos.X, o.X)
	}
}

func TestBulletBlockedByObstacle(t *testing.T) {
	g := newGame(nil, nil)
	o := g.obstacles[0]
	g.bullets = []*bullet{{
		owner: "x",
		pos:   vec{X: o.X - 5, Y: o.Y + o.H/2},
		vel:   vec{X: bulletSpeed, Y: 0}, // flying into the wall
		life:  bulletLife,
	}}
	g.updateBullets(0.05)
	if len(g.bullets) != 0 {
		t.Fatalf("bullet survived hitting an obstacle: %d remain", len(g.bullets))
	}
}

func TestForwardMovesTowardAim(t *testing.T) {
	g := newGame(nil, nil)
	g.addPlayer("a")
	p := g.players["a"]
	p.pos = vec{X: 500, Y: 500}

	// Aim straight up (screen -y): forward should move up, no sideways drift.
	g.applyInput("a", input{Seq: 1, Forward: true, AimX: 0, AimY: -1, Dt: maxInputDt})
	if p.pos.Y >= 500 {
		t.Fatalf("forward did not move toward aim: y=%v", p.pos.Y)
	}
	if math.Abs(p.pos.X-500) > 1e-9 {
		t.Fatalf("forward drifted sideways: x=%v", p.pos.X)
	}
}

func TestStrafeIsPerpendicular(t *testing.T) {
	g := newGame(nil, nil)
	g.addPlayer("a")
	p := g.players["a"]
	p.pos = vec{X: 500, Y: 500}

	// Aim up: strafing right should move +x (right on screen), no forward drift.
	g.applyInput("a", input{Seq: 1, StrafeRight: true, AimX: 0, AimY: -1, Dt: maxInputDt})
	if p.pos.X <= 500 {
		t.Fatalf("strafe right did not move +x: x=%v", p.pos.X)
	}
	if math.Abs(p.pos.Y-500) > 1e-9 {
		t.Fatalf("strafe drifted forward/back: y=%v", p.pos.Y)
	}
}

// fragVictim simulates the shooter landing one killing hit on the victim by
// placing a bullet on top of the victim and running collision.
func fragVictim(g *game, shooter, victim *player) {
	victim.alive = true
	victim.pos = vec{X: 100, Y: 100}
	b := &bullet{owner: shooter.id, pos: victim.pos}
	if !g.checkHit(b) {
		panic("expected hit")
	}
}

func TestMatchEndsAtWinScore(t *testing.T) {
	g := newGame(nil, nil)
	g.addPlayer("a")
	g.addPlayer("b")
	a, b := g.players["a"], g.players["b"]

	for i := 0; i < winScore; i++ {
		if g.phase != phasePlaying {
			t.Fatalf("match ended early at score %d", a.score)
		}
		fragVictim(g, a, b)
	}

	if g.phase != phaseEnded {
		t.Fatalf("phase = %q, want %q", g.phase, phaseEnded)
	}
	if g.winnerID != "a" {
		t.Fatalf("winner = %q, want a", g.winnerID)
	}
	if a.score != winScore {
		t.Fatalf("winner score = %d, want %d", a.score, winScore)
	}
}

func TestRoundTimerEndsWithLeaderWinning(t *testing.T) {
	g := newGame(nil, nil)
	g.addPlayer("a")
	g.addPlayer("b")
	g.players["a"].score = 3
	g.players["b"].score = 5

	g.update(roundDuration) // run the clock out

	if g.phase != phaseEnded {
		t.Fatalf("phase = %q, want %q", g.phase, phaseEnded)
	}
	if g.winnerID != "b" {
		t.Fatalf("winner = %q, want b (higher score)", g.winnerID)
	}
}

func TestRoundTimerTieIsADraw(t *testing.T) {
	g := newGame(nil, nil)
	g.addPlayer("a")
	g.addPlayer("b")
	g.players["a"].score = 4
	g.players["b"].score = 4

	g.update(roundDuration)

	if g.phase != phaseEnded {
		t.Fatalf("phase = %q, want ended", g.phase)
	}
	if g.winnerID != "" {
		t.Fatalf("winner = %q, want \"\" (draw)", g.winnerID)
	}
}

func TestResetRestoresRoundTimer(t *testing.T) {
	g := newGame(nil, nil)
	g.addPlayer("a")
	g.players["a"].score = winScore
	g.endMatch("a")
	g.update(matchResetDelay)

	if g.roundTimer != roundDuration {
		t.Fatalf("roundTimer = %v, want %v", g.roundTimer, roundDuration)
	}
}

func TestInputFrozenBetweenRounds(t *testing.T) {
	g := newGame(nil, nil)
	g.addPlayer("a")
	p := g.players["a"]
	p.pos = vec{X: 200, Y: 200}

	g.phase = phaseEnded
	// Forward with a rightward aim would move +x if the round were live.
	g.applyInput("a", input{Seq: 1, Forward: true, AimX: 1, Dt: maxInputDt})

	if p.pos.X != 200 {
		t.Fatalf("player moved during ended phase: x=%v", p.pos.X)
	}
	if p.lastSeq != 1 {
		t.Fatalf("input not acked during ended phase: lastSeq=%d", p.lastSeq)
	}
}

func TestResetStartsFreshRound(t *testing.T) {
	g := newGame(nil, nil)
	g.addPlayer("a")
	g.addPlayer("b")
	g.players["a"].score = winScore
	g.endMatch("a")

	// Not enough time elapsed yet.
	g.update(matchResetDelay - 0.1)
	if g.phase != phaseEnded {
		t.Fatal("match reset too early")
	}

	// Countdown elapses -> fresh round.
	g.update(0.2)
	if g.phase != phasePlaying {
		t.Fatalf("phase = %q, want %q", g.phase, phasePlaying)
	}
	if g.winnerID != "" {
		t.Fatalf("winner not cleared: %q", g.winnerID)
	}
	for id, p := range g.players {
		if p.score != 0 {
			t.Fatalf("player %s score not reset: %d", id, p.score)
		}
		if !p.alive {
			t.Fatalf("player %s not respawned", id)
		}
	}
}
