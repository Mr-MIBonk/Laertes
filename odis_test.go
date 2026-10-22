package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestInstallBridge(t *testing.T) {
	oe := filepath.Join(t.TempDir(), "OE")
	macros := filepath.Join(oe, "macros")
	if err := installBridge(macros); err == nil {
		t.Fatal("installed although ODIS's folder is missing")
	}
	os.Mkdir(oe, 0o755)
	os.Mkdir(macros, 0o755)
	os.WriteFile(filepath.Join(macros, "mcpserver_handlers.py"), []byte("old"), 0o644)
	if err := installBridge(macros); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]byte{"mcpserver.py": bridgeMacro, "mcpserver_handlers.py": bridgeHandlers} {
		got, err := os.ReadFile(filepath.Join(macros, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s not installed (%v)", name, err)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(macros, "*.laertes-tmp")); len(left) > 0 {
		t.Errorf("temporary files left: %v", left)
	}
}

func TestSoapFault(t *testing.T) {
	fault := `<?xml version='1.0' encoding='UTF-8'?><S:Envelope xmlns:S="http://schemas.xmlsoap.org/soap/envelope/"><S:Body><S:Fault><faultcode>S:Server</faultcode><faultstring>ODS8014E
Project not available.</faultstring></S:Fault></S:Body></S:Envelope>`
	if got := soapFault([]byte(fault)); got != "ODS8014E Project not available." {
		t.Errorf("fault = %q", got)
	}
	ok := `<S:Envelope xmlns:S="http://schemas.xmlsoap.org/soap/envelope/"><S:Body><ns2:exitResponse xmlns:ns2="x"/></S:Body></S:Envelope>`
	if got := soapFault([]byte(ok)); got != "" {
		t.Errorf("no fault expected, got %q", got)
	}
}
