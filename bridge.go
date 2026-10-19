package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// bridge is the connection to the ODIS Jython TCP bridge (mcpserver.py), which speaks
// newline-delimited JSON objects. The bridge serves one client at a time, so Laertes keeps a
// single connection open and sends its commands over it one after another.
type bridge struct {
	addr string
	mu   sync.Mutex
	conn net.Conn
	r    *bufio.Reader
}

// call sends one command to the bridge and returns its reply. A reply with "ok": false is an error.
func (b *bridge) call(cmd json.RawMessage) (json.RawMessage, error) {
	var line bytes.Buffer // one command per line, so it must not contain newlines
	if err := json.Compact(&line, cmd); err != nil {
		return nil, fmt.Errorf("bridge command: %w", err)
	}
	line.WriteByte('\n')

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != nil && !b.alive() {
		b.drop()
	}
	if b.conn == nil {
		conn, err := net.DialTimeout("tcp", b.addr, 2*time.Second)
		if err != nil {
			return nil, fmt.Errorf("ODIS bridge at %s is not reachable (run mcpserver.py in ODIS): %w", b.addr, err)
		}
		b.conn, b.r = conn, bufio.NewReader(conn)
	}
	// Some actions read from the control unit, which can take a while.
	b.conn.SetDeadline(time.Now().Add(2 * time.Minute))
	if _, err := b.conn.Write(line.Bytes()); err != nil {
		b.drop()
		return nil, fmt.Errorf("send to ODIS bridge: %w", err)
	}
	reply, err := b.r.ReadBytes('\n')
	if err != nil {
		b.drop()
		return nil, fmt.Errorf("read from ODIS bridge: %w", err)
	}
	var status struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(reply, &status); err != nil {
		b.drop()
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

// alive reports whether the open connection still works, before a command is sent on it: the
// bridge may have been restarted or may have turned this connection away. The bridge sends
// nothing unasked, so a read that times out at once means the connection is still open. Checking
// first, instead of retrying a failed command, never sends a command (a write to a unit) twice.
func (b *bridge) alive() bool {
	b.conn.SetReadDeadline(time.Now())
	_, err := b.conn.Read(make([]byte, 1))
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func (b *bridge) drop() {
	b.conn.Close()
	b.conn, b.r = nil, nil
}
