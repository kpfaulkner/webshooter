package main

import (
	"flag"
	"log"
	"net/http"
	"os"
)

func main() {
	levelsDir := flag.String("levels", "levels", "directory of level JSON files, played in filename order")
	flag.Parse()

	// ALLOWED_ORIGIN restricts which web origin may open a WebSocket. Leave it
	// unset for local dev (any origin); set it to your site (e.g.
	// https://your-app.fly.dev) in production. See checkOrigin in client.go.
	allowedOrigin = os.Getenv("ALLOWED_ORIGIN")

	levels, names, err := loadLevels(*levelsDir)
	if err != nil {
		log.Printf("levels: using built-in layout (%v)", err)
		levels, names = nil, nil // newGame falls back to defaultLevel
	} else {
		log.Printf("levels: loaded %d from %s (%v)", len(levels), *levelsDir, names)
	}

	hub := newHub(levels, names)
	go hub.run()

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		serveWs(hub, w, r)
	})
	// Serve client assets with no-store so the browser never runs a stale
	// game.js/index.html during development.
	mux.Handle("/", noCache(http.FileServer(http.Dir("static"))))

	// Hosting platforms (Fly.io, Render, etc.) inject the port via $PORT.
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	addr := ":" + port
	log.Printf("webshooter listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
