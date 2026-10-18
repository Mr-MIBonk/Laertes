package main

import (
	"encoding/binary"
	"fmt"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ODIS shows byte coding in a native list view (SWT Table): one row per byte with the columns
// byte no. | current hex | current binary | hex input | binary input. A new coding is entered
// the way a user would, by opening each hex input cell's editor and typing into it, so ODIS
// validates it and shows it next to the current value before anything is sent to the unit.

var (
	user32          = windows.NewLazySystemDLL("user32.dll")
	kernel32        = windows.NewLazySystemDLL("kernel32.dll")
	procWindowRect  = user32.NewProc("GetWindowRect")
	procPostMessage = user32.NewProc("PostMessageW")
	procSendMsgTO   = user32.NewProc("SendMessageTimeoutW")
	procAllocEx     = kernel32.NewProc("VirtualAllocEx")
	procFreeEx      = kernel32.NewProc("VirtualFreeEx")

	// Window enumeration reports into a package variable: a Go pointer handed to Windows as
	// the callback parameter goes stale when the goroutine stack moves during the callback.
	enumMu       sync.Mutex
	enumFound    []windows.HWND
	enumCallback = windows.NewCallback(func(h windows.HWND, _ uintptr) uintptr {
		enumFound = append(enumFound, h)
		return 1
	})
)

const (
	wmKeyDown          = 0x0100
	wmKeyUp            = 0x0101
	wmChar             = 0x0102
	wmClear            = 0x0303
	wmLButtonDown      = 0x0201
	wmLButtonUp        = 0x0202
	emSetSel           = 0x00B1
	vkReturn           = 0x0D
	lvmGetItemCount    = 0x1004
	lvmEnsureVisible   = 0x1013
	lvmGetSubItemRect  = 0x1038
	lvmGetItemTextW    = 0x1073
	colCurrentHex      = 1
	colHexInput        = 3
	remoteTextOffset   = 256
	remoteTextMaxChars = 100
)

type rect struct{ Left, Top, Right, Bottom int32 }

// fillHexInput types new into ODIS's hex input column for every byte that differs from the
// current value ODIS shows, clears it for the others (ODIS drops an input equal to the current
// value anyway), and reads the column back to confirm ODIS took the values.
// Unless old is nil, ODIS's current values must equal old, the coding new was derived from.
func (p *heapProc) fillHexInput(old, new []byte) (int, error) {
	lv, err := p.codingTable()
	if err != nil {
		return 0, err
	}
	remote, _, err := procAllocEx.Call(uintptr(p.h), 0, 4096, windows.MEM_COMMIT|windows.MEM_RESERVE, windows.PAGE_READWRITE)
	if remote == 0 {
		return 0, fmt.Errorf("allocate memory in ODIS: %w", err)
	}
	defer procFreeEx.Call(uintptr(p.h), remote, 0, windows.MEM_RELEASE)
	t := &listView{p, lv, remote}

	if n := int(send(lv, lvmGetItemCount, 0, 0)); n != len(new) {
		return 0, fmt.Errorf("ODIS byte coding table has %d bytes, the coding has %d; open the byte coding of this control unit in ODIS", n, len(new))
	}
	want := make([]string, len(new))
	for i := range new {
		cur, err := t.text(i, colCurrentHex)
		if err != nil {
			return 0, err
		}
		if old != nil && !strings.EqualFold(cur, fmt.Sprintf("%02X", old[i])) {
			return 0, fmt.Errorf("ODIS shows byte %d as %q but the original coding has %02X; open the byte coding of this control unit in ODIS", i, cur, old[i])
		}
		if v := fmt.Sprintf("%02X", new[i]); !strings.EqualFold(cur, v) {
			want[i] = v
		}
	}
	for i := range want {
		if cur, _ := t.text(i, colHexInput); cur != want[i] {
			if err := t.edit(i, colHexInput, want[i]); err != nil {
				return 0, fmt.Errorf("byte %d: %w", i, err)
			}
		}
	}

	// ODIS applies the editor value asynchronously, and can revert a cell when its editor
	// closes; accept only after every cell showed what was typed on two reads in a row.
	var bad []string
	for deadline, good := time.Now().Add(3*time.Second), 0; good < 2; time.Sleep(300 * time.Millisecond) {
		bad = bad[:0]
		for i := range want {
			if cur, _ := t.text(i, colHexInput); cur != want[i] {
				bad = append(bad, fmt.Sprintf("byte %d shows %q instead of %q", i, cur, want[i]))
			}
		}
		if len(bad) > 0 {
			good = 0
		} else {
			good++
		}
		if time.Now().After(deadline) {
			break
		}
	}
	if len(bad) > 0 {
		return 0, fmt.Errorf("ODIS did not take the new coding: %s", strings.Join(bad, ", "))
	}
	n := 0
	for _, w := range want {
		if w != "" {
			n++
		}
	}
	return n, nil
}

// codingTable finds the visible byte coding list view of the ODIS window.
func (p *heapProc) codingTable() (windows.HWND, error) {
	for _, h := range processWindows(p.pid) {
		if windowClass(h) == "SysListView32" && windows.IsWindowVisible(h) {
			return h, nil
		}
	}
	return 0, fmt.Errorf("ODIS byte coding table not found; open Coding of the control unit in ODIS and switch to binary coding")
}

// listView accesses a list view in ODIS through a scratch buffer allocated in the ODIS process,
// since list view messages read and write their structures in the owning process.
type listView struct {
	p      *heapProc
	h      windows.HWND
	remote uintptr
}

func (t *listView) text(row, col int) (string, error) {
	// LVITEMW (64-bit): iSubItem at 8, pszText at 24, cchTextMax at 32.
	item := make([]byte, 88)
	binary.LittleEndian.PutUint32(item[8:], uint32(col))
	binary.LittleEndian.PutUint64(item[24:], uint64(t.remote+remoteTextOffset))
	binary.LittleEndian.PutUint32(item[32:], remoteTextMaxChars)
	if err := t.p.write(t.remote, item); err != nil {
		return "", err
	}
	n := int(send(t.h, lvmGetItemTextW, uintptr(row), t.remote))
	if n == 0 {
		return "", nil
	}
	b, err := t.p.read(t.remote+remoteTextOffset, 2*n)
	if err != nil || len(b) < 2*n {
		return "", fmt.Errorf("read ODIS table cell: %v", err)
	}
	u := make([]uint16, n)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[2*i:])
	}
	return windows.UTF16ToString(u), nil
}

// cellRect returns the cell bounds in list view client coordinates.
func (t *listView) cellRect(row, col int) (rect, error) {
	send(t.h, lvmEnsureVisible, uintptr(row), 0)
	in := make([]byte, 16)
	binary.LittleEndian.PutUint32(in[4:], uint32(col)) // top = subitem, left = LVIR_BOUNDS
	if err := t.p.write(t.remote, in); err != nil {
		return rect{}, err
	}
	if send(t.h, lvmGetSubItemRect, uintptr(row), t.remote) == 0 {
		return rect{}, fmt.Errorf("ODIS table has no cell %d/%d", row, col)
	}
	b, err := t.p.read(t.remote, 16)
	if err != nil || len(b) < 16 {
		return rect{}, fmt.Errorf("read ODIS table cell bounds: %v", err)
	}
	return rect{int32(binary.LittleEndian.Uint32(b)), int32(binary.LittleEndian.Uint32(b[4:])),
		int32(binary.LittleEndian.Uint32(b[8:])), int32(binary.LittleEndian.Uint32(b[12:]))}, nil
}

// edit clicks a cell to open its editor, replaces the editor text and presses Enter.
func (t *listView) edit(row, col int, text string) error {
	cell, err := t.cellRect(row, col)
	if err != nil {
		return err
	}
	var table rect
	procWindowRect.Call(uintptr(t.h), uintptr(unsafe.Pointer(&table)))
	x, y := (cell.Left+cell.Right)/2, (cell.Top+cell.Bottom)/2
	// Screen position of the cell, to recognise its editor among ODIS's other text fields.
	sx, sy := table.Left+x, table.Top+y

	var editor windows.HWND
	for try := 0; try < 2 && editor == 0; try++ { // a second click opens the editor on some rows
		lParam := uintptr(uint16(y))<<16 | uintptr(uint16(x))
		procPostMessage.Call(uintptr(t.h), wmLButtonDown, 1, lParam)
		procPostMessage.Call(uintptr(t.h), wmLButtonUp, 0, lParam)
		for deadline := time.Now().Add(time.Second); editor == 0 && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			for _, h := range processWindows(t.p.pid) {
				var r rect
				procWindowRect.Call(uintptr(h), uintptr(unsafe.Pointer(&r)))
				if windowClass(h) == "Edit" && windows.IsWindowVisible(h) && r.Left <= sx && sx < r.Right+8 && r.Top-20 <= sy && sy <= r.Bottom+20 {
					editor = h
					break
				}
			}
		}
	}
	if editor == 0 {
		return fmt.Errorf("ODIS did not open the hex input editor")
	}
	send(editor, emSetSel, 0, ^uintptr(0))
	send(editor, wmClear, 0, 0)
	for _, c := range text {
		send(editor, wmChar, uintptr(c), 0)
	}
	send(editor, wmKeyDown, vkReturn, 0)
	send(editor, wmChar, vkReturn, 0)
	send(editor, wmKeyUp, vkReturn, 0)
	return nil
}

// send delivers a message to an ODIS window, giving up if ODIS hangs.
func send(h windows.HWND, msg uint32, wParam, lParam uintptr) uintptr {
	const smtoAbortIfHung = 0x0002
	var result uintptr
	procSendMsgTO.Call(uintptr(h), uintptr(msg), wParam, lParam, smtoAbortIfHung, 5000, uintptr(unsafe.Pointer(&result)))
	return result
}

// processWindows lists every window of process pid, top-level and child.
func processWindows(pid uint32) []windows.HWND {
	enumMu.Lock()
	defer enumMu.Unlock()
	enumFound = nil
	windows.EnumWindows(enumCallback, nil)
	var out []windows.HWND
	for _, top := range enumFound {
		var p uint32
		if windows.GetWindowThreadProcessId(top, &p); p == pid {
			out = append(out, top)
		}
	}
	tops := out
	for _, top := range tops {
		enumFound = nil
		windows.EnumChildWindows(top, enumCallback, nil)
		out = append(out, enumFound...)
	}
	return out
}

func windowClass(h windows.HWND) string {
	buf := make([]uint16, 64)
	n, _ := windows.GetClassName(h, &buf[0], int32(len(buf)))
	return windows.UTF16ToString(buf[:n])
}
