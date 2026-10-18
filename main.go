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
	"sort"
	"strconv"
	"strings"
	"time"
)

// version of Laertes, shown in the web page's top bar.
const version = "0.3.0"

//go:embed index.html
var indexHTML []byte

func main() {
	exe, _ := os.Executable()
	dbPath := flag.String("db", filepath.Join(filepath.Dir(exe), "laertes.db"), "SQLite database file")
	noBrowser := flag.Bool("nobrowser", false, "do not open the browser")
	bridgeAddr := flag.String("bridge", "127.0.0.1:8765", "ODIS Jython bridge (mcpserver.py) address")
	flag.Parse()
	adoptOldDatabase(*dbPath)

	store, err := openStore(*dbPath)
	if err != nil {
		log.Fatalf("database: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:23031")
	if err != nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		log.Fatal(err)
	}
	host := ln.Addr().String()
	url := "http://" + host + "/"
	log.Printf("database %s", *dbPath)
	log.Printf("Laertes %s", version)
	log.Printf("open %s", url)
	if !*noBrowser {
		exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	}
	log.Fatal(http.Serve(ln, sameOrigin(host, routes(store, *bridgeAddr, filepath.Dir(*dbPath)))))
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

func routes(store *Store, bridgeAddr, dataDir string) http.Handler {
	mux := http.NewServeMux()
	// Browse the ODIS PC for a file to flash: lists one directory (folders first). Laertes runs
	// on the same PC, so these are the paths the bridge can read.
	mux.HandleFunc("GET /api/files", func(w http.ResponseWriter, r *http.Request) {
		dir := r.URL.Query().Get("dir")
		if dir == "" {
			if h, err := os.UserHomeDir(); err == nil {
				dir = h
			} else {
				dir = dataDir
			}
		}
		dir = filepath.Clean(dir)
		ents, err := os.ReadDir(dir)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		type entry struct {
			Name string `json:"name"`
			Dir  bool   `json:"dir"`
			Size int64  `json:"size"`
		}
		list := []entry{}
		for _, e := range ents {
			info, _ := e.Info()
			var size int64
			if info != nil {
				size = info.Size()
			}
			list = append(list, entry{e.Name(), e.IsDir(), size})
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].Dir != list[j].Dir {
				return list[i].Dir
			}
			return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name)
		})
		reply(w, map[string]any{"dir": dir, "parent": filepath.Dir(dir), "entries": list}, nil)
	})
	// Upload a file from the browser into <dataDir>/uploads and return its path on the PC, so it
	// can be flashed. Streamed to disk; the file never has to exist on the PC beforehand.
	mux.HandleFunc("POST /api/upload", func(w http.ResponseWriter, r *http.Request) {
		name := filepath.Base(r.URL.Query().Get("name"))
		if name == "" || name == "." || name == string(filepath.Separator) || strings.ContainsAny(name, `/\`) {
			http.Error(w, "bad file name", http.StatusBadRequest)
			return
		}
		up := filepath.Join(dataDir, "uploads")
		if err := os.MkdirAll(up, 0o755); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		dst := filepath.Join(up, name)
		f, err := os.Create(dst)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer f.Close()
		if _, err := io.Copy(f, io.LimitReader(r.Body, 2<<30)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		reply(w, map[string]any{"path": dst, "name": name}, nil)
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		reply(w, version, nil)
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
	mux.HandleFunc("GET /api/retrofits", func(w http.ResponseWriter, r *http.Request) {
		rs, err := store.Retrofits()
		reply(w, rs, err)
	})
	mux.HandleFunc("GET /api/retrofits/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		rf, err := store.Retrofit(id)
		if err == nil && rf == nil {
			http.Error(w, "no such retrofit", http.StatusNotFound)
			return
		}
		reply(w, rf, err)
	})
	mux.HandleFunc("POST /api/retrofits", func(w http.ResponseWriter, r *http.Request) {
		var rf Retrofit
		// The baseline is a full-scan log of the whole car, so allow a large body.
		if err := json.NewDecoder(io.LimitReader(r.Body, 32<<20)).Decode(&rf); err != nil || rf.Name == "" ||
			(len(rf.BaselineDTCs) > 0 && !json.Valid(rf.BaselineDTCs)) {
			http.Error(w, "a retrofit needs a name", http.StatusBadRequest)
			return
		}
		rf.Started, rf.Ended = time.Now().Format(time.RFC3339), ""
		if err := store.StartRetrofit(&rf); err == errRetrofitActive {
			http.Error(w, err.Error(), http.StatusConflict)
		} else {
			reply(w, &rf, err)
		}
	})
	mux.HandleFunc("PATCH /api/retrofits/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		var req struct {
			Name string `json:"name"`
			End  bool   `json:"end"`
		}
		if err != nil || json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		ended := ""
		if req.End {
			ended = time.Now().Format(time.RFC3339)
		}
		reply(w, nil, store.UpdateRetrofit(id, strings.TrimSpace(req.Name), ended))
	})
	mux.HandleFunc("GET /api/settings", func(w http.ResponseWriter, r *http.Request) {
		v, err := store.Settings()
		reply(w, v, err)
	})
	mux.HandleFunc("PUT /api/settings", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		if err != nil || !json.Valid(body) {
			http.Error(w, "settings must be a JSON object", http.StatusBadRequest)
			return
		}
		reply(w, nil, store.SetSettings(body))
	})
	mux.HandleFunc("GET /api/coding-pref", func(w http.ResponseWriter, r *http.Request) {
		p, err := store.CodingPref(r.URL.Query().Get("variant"))
		if err != nil {
			reply(w, nil, err)
			return
		}
		if p == nil {
			reply(w, map[string]any{}, nil)
			return
		}
		reply(w, p, nil)
	})
	mux.HandleFunc("PUT /api/coding-pref", func(w http.ResponseWriter, r *http.Request) {
		var p CodingPref
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil || p.Variant == "" || p.BorrowVariant == "" {
			http.Error(w, "variant and borrowVariant are required", http.StatusBadRequest)
			return
		}
		reply(w, nil, store.SetCodingPref(p))
	})
	mux.HandleFunc("DELETE /api/coding-pref", func(w http.ResponseWriter, r *http.Request) {
		variant := r.URL.Query().Get("variant")
		if variant == "" {
			http.Error(w, "variant is required", http.StatusBadRequest)
			return
		}
		reply(w, nil, store.DeleteCodingPref(variant))
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
