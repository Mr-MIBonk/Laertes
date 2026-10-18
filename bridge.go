package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

// bridgeCall sends one command to the ODIS Jython TCP bridge (mcpserver.py), which speaks
// newline-delimited JSON objects, and returns its reply. A reply with "ok": false is an error.
func bridgeCall(addr string, cmd json.RawMessage) (json.RawMessage, error) {
	var line bytes.Buffer // one command per line, so it must not contain newlines
	if err := json.Compact(&line, cmd); err != nil {
		return nil, fmt.Errorf("bridge command: %w", err)
	}
	line.WriteByte('\n')

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("ODIS bridge at %s is not reachable (run mcpserver.py in ODIS): %w", addr, err)
	}
	defer conn.Close()
	// Some actions read from the control unit, which can take a while.
	conn.SetDeadline(time.Now().Add(2 * time.Minute))
	if _, err := conn.Write(line.Bytes()); err != nil {
		return nil, fmt.Errorf("send to ODIS bridge: %w", err)
	}
	reply, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("read from ODIS bridge: %w", err)
	}
	var status struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(reply, &status); err != nil {
		return nil, fmt.Errorf("ODIS bridge sent invalid JSON: %w", err)
	}
	if !status.OK {
		if status.Error == "" {
			status.Error = "request failed"
		}
		return nil, errors.New("ODIS bridge: " + status.Error)
	}
	return reply, nil
}
