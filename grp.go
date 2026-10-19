package main

import (
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

// grpReturns catches the end of ODIS's VW online (GRP) sign-in. ODIS signs in by OAuth in the
// user's browser, and VW sends the browser back to ODIS's fixed redirect address
// http://localhost/1?code=...&state=... (registered with VW for ODIS, so it cannot be changed).
// Laertes listens there, keeps the address by its state, and the page collects it
// (GET /api/grp-return) to hand it to ODIS. The code is useless without ODIS's PKCE verifier,
// which never leaves ODIS.
type grpReturns struct {
	mu   sync.Mutex
	urls map[string]grpReturn
}

type grpReturn struct {
	url string
	at  time.Time
}

// listen serves the redirect address on both loopback addresses (browsers try "localhost" as
// ::1 and 127.0.0.1). When port 80 is taken the page asks the user to paste the address instead.
func (g *grpReturns) listen() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /1", func(w http.ResponseWriter, r *http.Request) {
		state := r.URL.Query().Get("state")
		if state == "" {
			http.Error(w, "missing state", http.StatusBadRequest)
			return
		}
		g.put(state, "http://localhost/1?"+r.URL.RawQuery)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>Laertes</title>` +
			`<p style="font:16px sans-serif;margin:2em">Signed in. Laertes hands this to ODIS; you can close this tab.</p>`))
	})
	for _, addr := range []string{"127.0.0.1:80", "[::1]:80"} {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			log.Printf("VW online sign-in: cannot listen on %s (%v); the address will have to be pasted", addr, err)
			continue
		}
		go http.Serve(ln, mux)
	}
}

func (g *grpReturns) put(state, url string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.urls == nil {
		g.urls = map[string]grpReturn{}
	}
	for s, u := range g.urls { // abandoned sign-ins
		if time.Since(u.at) > time.Hour {
			delete(g.urls, s)
		}
	}
	g.urls[state] = grpReturn{url, time.Now()}
}

// take returns, once, the address the browser came back with for this state ("" until then).
func (g *grpReturns) take(state string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	u := g.urls[state]
	delete(g.urls, state)
	return u.url
}
