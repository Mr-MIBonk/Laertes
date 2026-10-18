package main

import (
	"bytes"
	"encoding/hex"
	"io"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Reading is one binary coding value that ODIS read from a control unit.
type Reading struct {
	ID        int64  `json:"id"`
	Time      string `json:"time"`
	ECU       string `json:"ecu"`
	Variant   string `json:"variant"`
	Subsystem string `json:"subsystem"`
	Coding    string `json:"coding"` // uppercase hex, no separators
}

var (
	reVariant = regexp.MustCompile(`ECU-Variant = <([^>]+)>`)
	reCoding  = regexp.MustCompile(`CodingUds\.getByteCodingValue.*subsystem <IdentifiedSystem\(name=(.*?)\s*,index=-?\d+\)>, coding value <(.*)$`)
	reLength  = regexp.MustCompile(`>, the length of the byte-array is <(\d+)>`)
	reEcu     = regexp.MustCompile(`Function <SelfdiagFunction\.coding\.name> was called on control unit <([^>]+)>`)
	reDumpRow = regexp.MustCompile(`^\s*[0-9A-Fa-f]{4}: (.*)$`)
	reHexByte = regexp.MustCompile(`\b[0-9A-Fa-f]{2}\b`)
)

// logParser turns ODIS engine.log lines into Readings.
// A coding hex dump longer than 16 bytes continues on following lines.
type logParser struct {
	variant string
	pending *Reading // coding dump still being collected
	dump    []string
	last    *Reading // last completed reading, waiting for its control unit name
	emit    func(*Reading)
	update  func(*Reading)
}

func (p *logParser) line(s string) {
	if p.pending != nil {
		p.dump = append(p.dump, s)
		p.tryFinish(s)
		return
	}
	if m := reVariant.FindStringSubmatch(s); m != nil {
		p.variant = m[1]
		return
	}
	if m := reCoding.FindStringSubmatch(s); m != nil {
		ts := ""
		if len(s) >= 21 {
			ts = s[:21]
		}
		p.pending = &Reading{Time: ts, Variant: p.variant, Subsystem: strings.TrimSpace(m[1])}
		p.dump = []string{m[2]}
		p.tryFinish(s)
		return
	}
	if m := reEcu.FindStringSubmatch(s); m != nil && p.last != nil && p.last.ECU == "" {
		p.last.ECU = m[1]
		p.update(p.last)
	}
}

func (p *logParser) tryFinish(s string) {
	m := reLength.FindStringSubmatch(s)
	if m == nil {
		if len(p.dump) > 64 { // malformed; give up on this dump
			p.pending, p.dump = nil, nil
		}
		return
	}
	n, _ := strconv.Atoi(m[1])
	var data []byte
	for _, row := range p.dump {
		row = reLength.Split(row, 2)[0]
		rm := reDumpRow.FindStringSubmatch(row)
		if rm == nil {
			continue
		}
		hexPart := rm[1]
		if len(hexPart) > 48 { // 16 bytes * "hh "; the ASCII column follows
			hexPart = hexPart[:48]
		}
		for _, h := range reHexByte.FindAllString(hexPart, -1) {
			b, _ := hex.DecodeString(h)
			data = append(data, b...)
		}
	}
	r := p.pending
	p.pending, p.dump = nil, nil
	if len(data) < n || n == 0 {
		log.Printf("could not parse coding dump of %s (%d of %d bytes)", r.Subsystem, len(data), n)
		return
	}
	r.Coding = strings.ToUpper(hex.EncodeToString(data[:n]))
	p.last = r
	p.emit(r)
}

// tailLog follows path forever, feeding complete lines to the parser.
// The file is opened only briefly on each poll so ODIS can still rotate it.
func tailLog(path string, p *logParser) {
	var offset int64
	var partial []byte
	for ; ; time.Sleep(500 * time.Millisecond) {
		f, err := openShared(path)
		if err != nil {
			continue
		}
		st, err := f.Stat()
		if err == nil && st.Size() < offset { // rotated
			offset, partial = 0, nil
		}
		if err == nil && st.Size() > offset {
			if _, err = f.Seek(offset, io.SeekStart); err == nil {
				var buf []byte
				buf, err = io.ReadAll(f)
				offset += int64(len(buf))
				buf = append(partial, buf...)
				lines := bytes.Split(buf, []byte("\n"))
				partial = append([]byte(nil), lines[len(lines)-1]...)
				for _, l := range lines[:len(lines)-1] {
					p.line(latin1(bytes.TrimRight(l, "\r")))
				}
			}
		}
		f.Close()
	}
}

func openShared(path string) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}

// ODIS writes its log in Windows-1252; Latin-1 is close enough for names.
func latin1(b []byte) string {
	r := make([]rune, len(b))
	for i, c := range b {
		r[i] = rune(c)
	}
	return string(r)
}
