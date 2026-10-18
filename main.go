package main

import (
	_ "embed"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

//go:embed index.html
var indexHTML []byte

func main() {
	exe, _ := os.Executable()
	logPath := flag.String("log", `C:\ProgramData\OE\log\engine.log`, "ODIS engine.log to follow")
	dbPath := flag.String("db", filepath.Join(filepath.Dir(exe), "laertes.db"), "SQLite database file")
	noBrowser := flag.Bool("nobrowser", false, "do not open the browser")
	bridgeAddr := flag.String("bridge", "127.0.0.1:8765", "ODIS Jython bridge (mcpserver_test.py) address")
	flag.Parse()
	adoptOldDatabase(*dbPath)

	store, err := openStore(*dbPath)
	if err != nil {
		log.Fatalf("database: %v", err)
	}

	p := &logParser{
		emit: func(r *Reading) {
			if err := store.AddReading(r); err != nil {
				log.Printf("save reading: %v", err)
				return
			}
			log.Printf("coding %s / %s: %s", r.Variant, r.Subsystem, r.Coding)
		},
		update: func(r *Reading) {
			if err := store.SetReadingECU(r); err != nil {
				log.Printf("save reading: %v", err)
			}
		},
	}
	go tailLog(*logPath, p)

	ln, err := net.Listen("tcp", "127.0.0.1:23031")
	if err != nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		log.Fatal(err)
	}
	host := ln.Addr().String()
	url := "http://" + host + "/"
	log.Printf("following %s", *logPath)
	log.Printf("database %s", *dbPath)
	log.Printf("open %s", url)
	if !*noBrowser {
		exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	}
	log.Fatal(http.Serve(ln, sameOrigin(host, routes(store, *bridgeAddr))))
}

// adoptOldDatabase renames the database of the tool's former name (odishelper.db, with its
// SQLite -wal and -shm files) to path, unless a database already exists at path.
func adoptOldDatabase(path string) {
	old := filepath.Join(filepath.Dir(path), "odishelper.db")
	if _, err := os.Stat(path); err == nil {
		return
	}
	if _, err := os.Stat(old); err != nil {
		return
	}
	// All files move or none: a database separated from its -wal file loses recent changes.
	var moved []string
	for _, suffix := range []string{"", "-wal", "-shm"} {
		err := os.Rename(old+suffix, path+suffix)
		if err == nil {
			moved = append(moved, suffix)
		} else if !os.IsNotExist(err) {
			for _, m := range moved {
				os.Rename(path+m, old+m)
			}
			log.Fatalf("rename %s to %s (is the old odishelper.exe still running?): %v", old+suffix, path+suffix, err)
		}
	}
	log.Printf("renamed %s to %s", old, path)
}

// sameOrigin rejects requests from other web pages (CSRF) and DNS-rebinding hosts,
// since this API can write into the ODIS process.
func sameOrigin(host string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if r.Host != host || (r.Method != http.MethodGet && origin != "http://"+host) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func routes(store *Store, bridgeAddr string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/readings", func(w http.ResponseWriter, r *http.Request) {
		rs, err := store.Readings(200)
		reply(w, rs, err)
	})
	mux.HandleFunc("DELETE /api/readings/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		reply(w, nil, store.DeleteReadings(id))
	})
	mux.HandleFunc("DELETE /api/readings", func(w http.ResponseWriter, r *http.Request) {
		reply(w, nil, store.DeleteReadings(0))
	})
	mux.HandleFunc("GET /api/labels", func(w http.ResponseWriter, r *http.Request) {
		ls, err := store.Labels(r.URL.Query().Get("variant"), r.URL.Query().Get("subsystem"))
		reply(w, ls, err)
	})
	mux.HandleFunc("PUT /api/labels", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Variant   string  `json:"variant"`
			Subsystem string  `json:"subsystem"`
			Labels    []Label `json:"labels"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		for _, l := range req.Labels {
			if !l.valid() {
				http.Error(w, "bad label", http.StatusBadRequest)
				return
			}
		}
		reply(w, nil, store.SetLabels(req.Variant, req.Subsystem, req.Labels))
	})
	mux.HandleFunc("POST /api/labels/import", func(w http.ResponseWriter, r *http.Request) {
		src, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ls, err := parseLBL(latin1(src))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		q := r.URL.Query()
		reply(w, len(ls), store.SetLabels(q.Get("variant"), q.Get("subsystem"), ls))
	})
	mux.HandleFunc("GET /api/history", func(w http.ResponseWriter, r *http.Request) {
		cs, err := store.Changes(500)
		reply(w, cs, err)
	})
	mux.HandleFunc("POST /api/history", func(w http.ResponseWriter, r *http.Request) {
		var c Change
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if c.Time == "" {
			c.Time = time.Now().Format(time.RFC3339)
		}
		reply(w, &c, store.AddChange(&c))
	})
	mux.HandleFunc("DELETE /api/history/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		reply(w, nil, store.DeleteChanges(id))
	})
	mux.HandleFunc("DELETE /api/history", func(w http.ResponseWriter, r *http.Request) {
		reply(w, nil, store.DeleteChanges(0))
	})
	mux.HandleFunc("POST /api/bridge", func(w http.ResponseWriter, r *http.Request) {
		var cmd struct {
			Action string `json:"action"`
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		if err != nil || json.Unmarshal(body, &cmd) != nil || cmd.Action == "" {
			http.Error(w, "bridge command must be a JSON object with an action", http.StatusBadRequest)
			return
		}
		res, err := bridgeCall(bridgeAddr, body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(res)
	})
	return mux
}

func reply(w http.ResponseWriter, v any, err error) {
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
