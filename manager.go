package main

import "sync"

// Manager owns the set of game rooms. Each distinct password maps to its own
// Hub — an independent world with its own tick loop — so players who share a
// password play together and are isolated from everyone else. Rooms are created
// on demand when the first player for a password connects, and reaped once the
// last player leaves so unused rooms don't accumulate.
type Manager struct {
	mu     sync.Mutex
	hubs   map[string]*Hub
	levels [][]rect
	names  []string
}

func newManager(levels [][]rect, names []string) *Manager {
	return &Manager{
		hubs:   make(map[string]*Hub),
		levels: levels,
		names:  names,
	}
}

// acquire returns the hub for password, creating and starting it if none exists
// yet, and records a pending join. The pending count keeps a brand-new (or
// about-to-be-empty) room from being reaped in the window between handing out
// the hub here and the client actually registering on it. Every acquire must be
// balanced by exactly one registered call.
func (m *Manager) acquire(password string) *Hub {
	m.mu.Lock()
	defer m.mu.Unlock()
	h := m.hubs[password]
	if h == nil {
		h = newHub(m.levels, m.names)
		h.manager = m
		h.password = password
		m.hubs[password] = h
		go h.run()
	}
	h.pending++
	return h
}

// registered clears the pending join recorded by acquire, once the client has
// been added to the hub's client set. Called from the hub goroutine.
func (m *Manager) registered(h *Hub) {
	m.mu.Lock()
	h.pending--
	m.mu.Unlock()
}

// reapIfEmpty removes the hub from the registry when it has no clients and no
// pending joins, returning true so the caller (the hub's run loop) knows to
// stop ticking. Called from the hub goroutine, which is the only writer of
// h.clients, so reading it here under the manager lock is safe.
func (m *Manager) reapIfEmpty(h *Hub) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(h.clients) == 0 && h.pending == 0 {
		delete(m.hubs, h.password)
		return true
	}
	return false
}
