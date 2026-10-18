package main

import (
	"fmt"
	"strconv"
	"strings"
)

// parseLBL reads the long-coding ("LC") lines of a Ross-Tech VCDS label file:
//
//	LC,00,0=,text        general note
//	LC,05,2,text         bit 2 of byte 5
//	LC,01,0~2,05,text    bits 0..2 of byte 1 equal to 0x05
//
// Other line types (security access, measuring blocks, ...) are ignored.
func parseLBL(src string) ([]Label, error) {
	var out []Label
	notes := 0
	for n, line := range strings.Split(strings.ReplaceAll(src, "\r", ""), "\n") {
		f := strings.SplitN(line, ",", 4)
		if len(f) < 4 || f[0] != "LC" {
			continue
		}
		bad := fmt.Errorf("line %d: cannot read %q", n+1, line)
		byteNo, err := strconv.Atoi(f[1])
		if err != nil {
			return nil, bad
		}
		bits := f[2]
		switch {
		case strings.HasSuffix(bits, "="):
			out = append(out, Label{Byte: -1, Lo: notes, Hi: notes, Value: -1, Text: strings.TrimSpace(f[3])})
			notes++
		case strings.Contains(bits, "~"):
			lo, err1 := strconv.Atoi(strings.Split(bits, "~")[0])
			hi, err2 := strconv.Atoi(strings.Split(bits, "~")[1])
			vt := strings.SplitN(f[3], ",", 2)
			if err1 != nil || err2 != nil || len(vt) != 2 {
				return nil, bad
			}
			v, err := strconv.ParseUint(vt[0], 16, 8)
			if err != nil {
				return nil, bad
			}
			out = append(out, Label{Byte: byteNo, Lo: lo, Hi: hi, Value: int(v), Text: strings.TrimSpace(vt[1])})
		default:
			bit, err := strconv.Atoi(bits)
			if err != nil {
				return nil, bad
			}
			out = append(out, Label{Byte: byteNo, Lo: bit, Hi: bit, Value: -1, Text: strings.TrimSpace(f[3])})
		}
		if l := out[len(out)-1]; !l.valid() || l.Text == "" {
			return nil, bad
		}
	}
	return out, nil
}
