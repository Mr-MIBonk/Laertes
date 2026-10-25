package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const singleAccessMsg = "Single access limit: Laertes is already open in another browser window or tab. " +
	"Close it there, then this page continues."

// clientLease is how long the page using Laertes keeps it after its last request or ping while it
// has no live socket (a page that went away without closing its socket, or one still loading).
const clientLease = 30 * time.Second

// pingEvery is how often Laertes tells the page over its socket that it is up.
const pingEvery = 5 * time.Second

// owner is the one page allowed to use the API: two pages driving ODIS at once would interfere on
// the vehicle. A page holds it while its socket (/api/ws) is open, and for clientLease after its
// last request or ping; its socket closing (page closed or reloaded) frees it at once. Another
// page can take it over (take); the owner's socket is then closed at once.
type owner struct {
	mu   sync.Mutex
	id   string
	seen time.Time
	kick chan struct{} // closed to end the owner's socket when another page takes over
}

// claim makes id the owner unless another page holds it, and reports whether id owns it now.
func (o *owner) claim(id string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.id != "" && o.id != id && time.Since(o.seen) <= clientLease {
		return false
	}
	o.id, o.seen = id, time.Now()
	return true
}

// take makes id the owner even while another page holds it, ending that page's socket.
func (o *owner) take(id string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.id != id && o.kick != nil {
		close(o.kick)
		o.kick = nil
	}
	o.id, o.seen = id, time.Now()
}

// attach registers the owner's socket; the returned channel closes when another page takes over.
// It reports false when id does not own Laertes (any more).
func (o *owner) attach(id string) (<-chan struct{}, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.id != id {
		return nil, false
	}
	if o.kick != nil {
		close(o.kick) // an older socket of the same page
	}
	o.kick = make(chan struct{})
	return o.kick, true
}

// release frees Laertes when the socket that ends (kick, from attach) is still the owner's.
func (o *owner) release(id string, kick <-chan struct{}) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.id == id && o.kick == kick {
		o.id = ""
		o.kick = nil
	}
}

// claimOrTake claims, or takes over when the page asks for it ("Use Laertes here").
func (o *owner) claimOrTake(id string, take bool) bool {
	if take {
		o.take(id)
		return true
	}
	return o.claim(id)
}

// singleClient lets only the owner use the API. Each page sends its own random id, in
// X-Laertes-Client (or ?client= on the socket, which cannot carry headers); another page's
// requests are refused with 409, and its socket is closed with code 4409. X-Laertes-Take: 1 (or
// ?take=1) takes over from the page that has it.
func singleClient(next http.Handler) http.Handler {
	var o owner
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/api/ws" {
			serveSocket(w, r, &o)
			return
		}
		id := r.Header.Get("X-Laertes-Client")
		if id == "" {
			http.Error(w, "missing X-Laertes-Client header", http.StatusBadRequest)
			return
		}
		if !o.claimOrTake(id, r.Header.Get("X-Laertes-Take") == "1") {
			http.Error(w, singleAccessMsg, http.StatusConflict)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// serveSocket is the page's live link to Laertes: it sends {"type":"ping","version":…} every
// pingEvery so the page can show whether Laertes is running, and holds the page's ownership
// while it is open. The page sends nothing.
func serveSocket(w http.ResponseWriter, r *http.Request, o *owner) {
	id := r.URL.Query().Get("client")
	if id == "" {
		http.Error(w, "missing client", http.StatusBadRequest)
		return
	}
	c, err := websocket.Accept(w, r, nil) // checks that the page is Laertes's own (Origin = Host)
	if err != nil {
		return
	}
	defer c.CloseNow()
	var kick <-chan struct{}
	ok := o.claimOrTake(id, r.URL.Query().Get("take") == "1")
	if ok {
		kick, ok = o.attach(id)
	}
	if !ok {
		c.Close(4409, "single access limit") // a close reason is limited to 123 bytes
		return
	}
	defer o.release(id, kick)
	log.Printf("page connected (%s)", r.RemoteAddr)
	defer log.Printf("page disconnected (%s)", r.RemoteAddr)
	ctx := c.CloseRead(r.Context()) // done when the page closes the socket
	ping, _ := json.Marshal(map[string]string{"type": "ping", "version": version})
	t := time.NewTicker(pingEvery)
	defer t.Stop()
	for {
		wctx, cancel := context.WithTimeout(ctx, pingEvery)
		err := c.Write(wctx, websocket.MessageText, ping)
		cancel()
		if err != nil || !o.claim(id) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-kick:
			log.Printf("page taken over by another page (%s)", r.RemoteAddr)
			c.Close(4409, "taken over by another page")
			return
		case <-t.C:
		}
	}
}
