package main

import (
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	mux.HandleFunc("POST /api/odis/find", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Coding string }
		data, ok := decodeCoding(w, r, &req, &req.Coding)
		if !ok {
			return
		}
		p, err := openODIS(false)
		if err != nil {
			reply(w, nil, err)
			return
		}
		defer p.Close()
		addrs, err := p.findByteArrays(data)
		hexAddrs := make([]string, len(addrs))
		for i, a := range addrs {
			hexAddrs[i] = fmt.Sprintf("%#x", a)
		}
		reply(w, map[string]any{"pid": p.pid, "addresses": hexAddrs}, err)
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
	mux.HandleFunc("POST /api/odis/write", func(w http.ResponseWriter, r *http.Request) {
		// From, if given, is the coding To was derived from; ODIS must still show it.
		var req struct{ From, To string }
		to, ok := decodeCoding(w, r, &req, &req.To)
		if !ok {
			return
		}
		var from []byte
		if req.From != "" {
			var err error
			if from, err = hex.DecodeString(req.From); err != nil || len(from) != len(to) {
				http.Error(w, "original coding must be hex of the same length", http.StatusBadRequest)
				return
			}
		}
		p, err := openODIS(true)
		if err != nil {
			reply(w, nil, err)
			return
		}
		defer p.Close()
		n, err := p.fillHexInput(from, to)
		if err != nil {
			log.Printf("ODIS hex input: %v", err)
		} else {
			log.Printf("ODIS hex input: entered %X (%d bytes differ from ODIS's current value)", to, n)
		}
		reply(w, map[string]any{"pid": p.pid, "changed": n}, err)
	})
	return mux
}

// decodeCoding decodes the JSON body into req and the hex coding field it points to.
func decodeCoding(w http.ResponseWriter, r *http.Request, req any, field *string) ([]byte, bool) {
	if err := json.NewDecoder(r.Body).Decode(req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return nil, false
	}
	data, err := hex.DecodeString(*field)
	if err != nil || len(data) == 0 {
		http.Error(w, "coding must be hex", http.StatusBadRequest)
		return nil, false
	}
	return data, true
}

func reply(w http.ResponseWriter, v any, err error) {
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
