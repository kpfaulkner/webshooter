package main

import "testing"

func TestManagerRoutesByPassword(t *testing.T) {
	m := newManager(nil, nil)

	h1 := m.acquire("alpha")
	h1b := m.acquire("alpha")
	h2 := m.acquire("beta")

	if h1 != h1b {
		t.Fatal("same password should share a hub")
	}
	if h1 == h2 {
		t.Fatal("different passwords should get separate hubs")
	}

	// Balance the acquires and let the now-idle rooms reap themselves so the
	// tick goroutines exit cleanly rather than leaking. Sending an unknown client
	// to unregister is a no-op join-wise but drives the reap check.
	m.registered(h1)
	m.registered(h1)
	m.registered(h2)
	h1.unregister <- &Client{}
	h2.unregister <- &Client{}
}

func TestRoomReapingGuardedByPending(t *testing.T) {
	m := newManager(nil, nil)
	h := newHub(nil, nil)
	h.manager = m
	h.password = "room"
	m.hubs["room"] = h

	// A join is in flight (acquire bumped pending): the room must survive even
	// though it has no clients yet, or a racing reconnect would lose its hub.
	h.pending = 1
	if m.reapIfEmpty(h) {
		t.Fatal("reaped a room with a join still in flight")
	}
	if _, ok := m.hubs["room"]; !ok {
		t.Fatal("room removed from registry despite a pending join")
	}

	// Join completed (pending cleared) with a member present: stays alive.
	h.pending = 0
	h.clients[&Client{}] = true
	if m.reapIfEmpty(h) {
		t.Fatal("reaped a room that still has a client")
	}

	// Last client left and nothing pending: now it's reaped and dropped.
	h.clients = map[*Client]bool{}
	if !m.reapIfEmpty(h) {
		t.Fatal("did not reap an empty, idle room")
	}
	if _, ok := m.hubs["room"]; ok {
		t.Fatal("reaped room still in registry")
	}
}
