package main

import (
	"bufio"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeBridge serves the bridge's line protocol on a local port: each command line is answered with
// replies[action], and every command is recorded.
func fakeBridge(t *testing.T, replies map[string]string) (addr string, got *[]map[string]any) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got = &[]map[string]any{}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				sc := bufio.NewScanner(conn)
				for sc.Scan() {
					var cmd map[string]any
					json.Unmarshal(sc.Bytes(), &cmd)
					*got = append(*got, cmd)
					action, _ := cmd["action"].(string)
					conn.Write([]byte(replies[action] + "\n"))
				}
			}()
		}
	}()
	return ln.Addr().String(), got
}

func postBridge(h http.Handler, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/api/bridge", strings.NewReader(body)))
	return w
}

// The Project units tab lists the session vehicle's units (list_vehicle_ecus) and, when ODIS has no
// session vehicle, the project's units (list_project_ecus). The page gets the bridge's reply as it
// is, in the fields it reads (repr, read_state, odx_variant, system_name.identification, dtc_count);
// a failed reply becomes an error that makes it fall back.
func TestProjectUnitsOutput(t *testing.T) {
	ecus := `{"ok":true,"ecus":[` +
		`{"repr":"0019 - Gateway (J533) 5Q0907530AA 1234","hw_part":"5Q0907530AA","sw_part":"5Q0907530AA","hw_version":"H07","sw_version":"1234",` +
		`"protocol":"UDS","odx_variant":"EV_GatewNF_001","read_state":"READ","available":true,"system_name":{"identification":"GW MQB  "},"serial":"123","dtc_count":2},` +
		`{"repr":"0001 - Engine (J623) --- ---","hw_part":"---","sw_part":"---","hw_version":null,"sw_version":null,` +
		`"protocol":null,"odx_variant":null,"read_state":"NOT_READ","available":false,"system_name":null,"serial":null,"dtc_count":null}]}`
	project := `{"ok":true,"count":1,"ecus":[{"repr":"0019 - Gateway","index":0}]}`
	addr, got := fakeBridge(t, map[string]string{
		"list_vehicle_ecus": ecus,
		"list_project_ecus": project,
		"vehicle":           `{"ok":false,"error":"ISessionVehicleDataProvider not found"}`,
		"broken":            `not json`,
	})
	h := routes(nil, &bridge{addr: addr}, nil, nil, t.TempDir())

	w := postBridge(h, `{"action":"list_vehicle_ecus"}`)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" || w.Body.String() != ecus+"\n" {
		t.Errorf("list_vehicle_ecus -> %d %q:\n%s", w.Code, w.Header().Get("Content-Type"), w.Body)
	}

	w = postBridge(h, "{\n  \"action\": \"list_project_ecus\",\n  \"include_methods\": false\n}")
	if w.Code != http.StatusOK || w.Body.String() != project+"\n" {
		t.Errorf("list_project_ecus -> %d:\n%s", w.Code, w.Body)
	}
	if last := (*got)[len(*got)-1]; last["include_methods"] != false {
		t.Errorf("bridge got %v, want include_methods false passed on", last)
	}

	w = postBridge(h, `{"action":"vehicle"}`)
	if w.Code != http.StatusBadGateway || strings.TrimSpace(w.Body.String()) != "ODIS bridge: ISessionVehicleDataProvider not found" {
		t.Errorf("failed reply -> %d %q", w.Code, w.Body)
	}
	w = postBridge(h, `{"action":"broken"}`)
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "invalid JSON") {
		t.Errorf("invalid reply -> %d %q", w.Code, w.Body)
	}
	for _, body := range []string{`{}`, `[]`, `nope`} {
		if w := postBridge(h, body); w.Code != http.StatusBadRequest {
			t.Errorf("command %s -> %d, want 400", body, w.Code)
		}
	}
	// The connection, dropped after the invalid reply, is opened again for the next command.
	if w := postBridge(h, `{"action":"list_vehicle_ecus"}`); w.Code != http.StatusOK {
		t.Errorf("after a reconnect -> %d %q", w.Code, w.Body)
	}
}
