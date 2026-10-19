package main

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// protect encrypts data with Windows DPAPI for the current Windows user, so a secret kept in
// laertes.db is useless when the file is copied off this account.
func protect(data []byte) ([]byte, error) {
	return dpapi(data, true)
}

// unprotect decrypts what protect returned.
func unprotect(data []byte) ([]byte, error) {
	return dpapi(data, false)
}

// dpapi runs data through CryptProtectData (encrypt) or CryptUnprotectData and frees the buffer
// Windows returns.
func dpapi(data []byte, encrypt bool) ([]byte, error) {
	if len(data) == 0 {
		return nil, nil
	}
	in := windows.DataBlob{Size: uint32(len(data)), Data: &data[0]}
	var out windows.DataBlob
	var err error
	if encrypt {
		err = windows.CryptProtectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	} else {
		err = windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	}
	if err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}
