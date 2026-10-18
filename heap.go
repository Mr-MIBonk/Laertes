package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ODIS runs on a 64-bit HotSpot JVM (Java 17, compressed class pointers).
// A byte[] object is: mark word (8) | klass (4) | length (4) | data, 8-byte aligned,
// so the length field sits at addr%8 == 4 and the data at addr%8 == 0.

const (
	odisProcess = "OffboardDiagLauncher.exe"
	memPrivate  = 0x20000
)

type heapProc struct {
	pid uint32
	h   windows.Handle
}

func openODIS(write bool) (*heapProc, error) {
	pid, err := findPID(odisProcess)
	if err != nil {
		return nil, err
	}
	access := uint32(windows.PROCESS_QUERY_INFORMATION | windows.PROCESS_VM_READ)
	if write {
		access |= windows.PROCESS_VM_WRITE | windows.PROCESS_VM_OPERATION
	}
	h, err := windows.OpenProcess(access, false, pid)
	if err != nil {
		return nil, fmt.Errorf("open %s (pid %d): %w", odisProcess, pid, err)
	}
	return &heapProc{pid, h}, nil
}

func (p *heapProc) Close() { windows.CloseHandle(p.h) }

func findPID(name string) (uint32, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if windows.UTF16ToString(e.ExeFile[:]) == name {
			return e.ProcessID, nil
		}
	}
	return 0, fmt.Errorf("%s is not running", name)
}

// findByteArrays returns the data addresses of every live byte[] whose contents equal data.
func (p *heapProc) findByteArrays(data []byte) ([]uintptr, error) {
	pat := binary.LittleEndian.AppendUint32(nil, uint32(len(data)))
	pat = append(pat, data...)
	var hits []uintptr
	err := p.scan(func(base uintptr, buf []byte) {
		for off := 0; ; {
			i := bytes.Index(buf[off:], pat)
			if i < 0 {
				return
			}
			addr := base + uintptr(off+i) + 4 // start of array data
			if addr%8 == 0 {
				hits = append(hits, addr)
			}
			off += i + 1
		}
	})
	return hits, err
}

// scan calls fn for every committed, private, read-write region (where the Java heap lives).
func (p *heapProc) scan(fn func(base uintptr, buf []byte)) error {
	const chunk = 16 << 20
	buf := make([]byte, chunk)
	var mbi windows.MemoryBasicInformation
	for addr := uintptr(0); ; addr = mbi.BaseAddress + mbi.RegionSize {
		if err := windows.VirtualQueryEx(p.h, addr, &mbi, unsafe.Sizeof(mbi)); err != nil {
			return nil // past the end of the address space
		}
		if mbi.State != windows.MEM_COMMIT || mbi.Type != memPrivate || mbi.Protect&windows.PAGE_READWRITE == 0 || mbi.Protect&windows.PAGE_GUARD != 0 {
			continue
		}
		// Chunks overlap by 512 bytes so a match is never split across two reads.
		for off := uintptr(0); off < mbi.RegionSize; off += chunk - 512 {
			n := min(uintptr(chunk), mbi.RegionSize-off)
			var read uintptr
			if windows.ReadProcessMemory(p.h, mbi.BaseAddress+off, &buf[0], n, &read) != nil || read == 0 {
				continue
			}
			fn(mbi.BaseAddress+off, buf[:read])
			if n < chunk {
				break
			}
		}
	}
}

func (p *heapProc) read(addr uintptr, n int) ([]byte, error) {
	buf := make([]byte, n)
	var read uintptr
	if err := windows.ReadProcessMemory(p.h, addr, &buf[0], uintptr(n), &read); err != nil {
		return nil, err
	}
	return buf[:read], nil
}

func (p *heapProc) write(addr uintptr, data []byte) error {
	var written uintptr
	if err := windows.WriteProcessMemory(p.h, addr, &data[0], uintptr(len(data)), &written); err != nil {
		return fmt.Errorf("write ODIS memory: %w", err)
	}
	return nil
}
