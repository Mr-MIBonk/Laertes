package main

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// The offline VCI (offlinevci/offlinevci.c): a J2534 device without hardware, so ODIS loads a
// vehicle project and runs the bridge macro with no interface connected.
//
//go:embed offlinevci/laertes_vci32.dll
var offlineVCIDLL []byte

const (
	offlineVCIName = "Laertes-Offline" // the device name ODIS shows and stores in vci.properties
	offlineVCIKey  = `SOFTWARE\WOW6432Node\PassThruSupport.04.04\Laertes - Offline VCI`
)

var offlineVCIPath = filepath.Join(odisDir, "laertes", "laertes_vci32.dll")

// offlineConfPath is the simulation configuration the offline VCI reads (offlinevci.c loadConfig),
// written next to the DLL so the DLL finds it in its own folder.
var offlineConfPath = filepath.Join(odisDir, "laertes", "offline.conf")

// offlineCANLogPath is the CAN trace the offline VCI appends to (offlinevci.c canLog), read by the
// "VCI CAN" monitor. It is next to the DLL, in the DLL's own folder.
var offlineCANLogPath = filepath.Join(odisDir, "laertes", "vci-can.log")

// readCANLog returns the recent lines of the VCI CAN trace (the VCI caps the file, so it is small),
// newest last, empty when there is no trace yet.
func readCANLog() []string {
	data, err := os.ReadFile(offlineCANLogPath)
	if err != nil {
		return []string{}
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	const max = 4000
	if len(lines) > max {
		lines = lines[len(lines)-max:]
	}
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// clearCANLog empties the VCI CAN trace (truncates; the VCI reopens it on its next message).
func clearCANLog() error {
	err := os.WriteFile(offlineCANLogPath, nil, 0o644)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// writeOfflineConfig writes the simulation settings the offline VCI reads: the VIN the simulated
// units report (DID 0xF190). An empty vin omits the line, so the VCI keeps its built-in default.
func writeOfflineConfig(vin string) error {
	var b strings.Builder
	if vin = strings.TrimSpace(vin); vin != "" {
		fmt.Fprintf(&b, "vin=%s\n", vin)
	}
	if err := os.MkdirAll(filepath.Dir(offlineConfPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(offlineConfPath, []byte(b.String()), 0o644)
}

// installOfflineVCI copies the DLL into place and registers it as a J2534 device. Registering
// writes HKLM, so Windows asks for administrator rights the first time.
func installOfflineVCI() error {
	if old, err := os.ReadFile(offlineVCIPath); err != nil || !bytes.Equal(old, offlineVCIDLL) {
		if err := os.MkdirAll(filepath.Dir(offlineVCIPath), 0o755); err != nil {
			return err
		}
		// A DLL that ODIS has loaded cannot be overwritten; the one in place then stays.
		if err := os.WriteFile(offlineVCIPath, offlineVCIDLL, 0o644); err != nil {
			if _, inPlace := os.Stat(offlineVCIPath); inPlace != nil {
				return err
			}
		}
	}
	if offlineVCIRegistered() {
		return nil
	}
	reg := filepath.Join(filepath.Dir(offlineVCIPath), "register.reg")
	if err := os.WriteFile(reg, []byte(offlineVCIReg()), 0o644); err != nil {
		return err
	}
	// Start-Process -Verb RunAs shows the UAC prompt and waits for reg.exe to finish.
	cmd := exec.Command("powershell", "-NoProfile", "-Command",
		fmt.Sprintf(`Start-Process reg.exe -ArgumentList 'import','"%s"' -Verb RunAs -Wait -WindowStyle Hidden`, reg))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("register the offline VCI (administrator rights refused?): %v %s", err, strings.TrimSpace(string(out)))
	}
	if !offlineVCIRegistered() {
		return errors.New("the offline VCI was not registered (administrator rights refused?)")
	}
	return nil
}

func offlineVCIRegistered() bool {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, offlineVCIKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	lib, _, err := k.GetStringValue("FunctionLibrary")
	return err == nil && strings.EqualFold(lib, offlineVCIPath)
}

// offlineVCIReg is the registry file that registers the offline VCI (UTF-16 is not needed: reg
// import reads "REGEDIT4" files as ANSI, and the paths are ASCII).
func offlineVCIReg() string {
	esc := strings.ReplaceAll(offlineVCIPath, `\`, `\\`)
	return "REGEDIT4\r\n\r\n[HKEY_LOCAL_MACHINE\\" + offlineVCIKey + "]\r\n" +
		`"Vendor"="Laertes"` + "\r\n" +
		`"Name"="` + offlineVCIName + `"` + "\r\n" +
		`"FunctionLibrary"="` + esc + `"` + "\r\n" +
		`"CAN"=dword:00000001` + "\r\n" + `"ISO15765"=dword:00000001` + "\r\n" +
		`"ISO9141"=dword:00000001` + "\r\n" + `"ISO14230"=dword:00000001` + "\r\n"
}

// ODIS keeps the VCI to use in vci.properties of the Windows user. An offline session points it
// at the offline VCI and keeps the user's choice in a backup, put back once that ODIS has ended.
func vciProperties() string {
	return filepath.Join(odisDir, "config", os.Getenv("USERNAME"), "vci.properties")
}

func useOfflineVCI() error {
	path := vciProperties()
	backup := path + ".laertes-online"
	if _, err := os.Stat(backup); os.IsNotExist(err) {
		if cur, err := os.ReadFile(path); err == nil {
			if err := os.WriteFile(backup, cur, 0o644); err != nil {
				return err
			}
		}
	}
	return os.WriteFile(path, []byte("#Written by Laertes for an offline session; the previous file is vci.properties.laertes-online\r\n"+
		"type_of_vci_selection=DEVICE\r\nvci_modulename="+offlineVCIName+"\r\nvci_serialnumber=\r\n"+
		"hint_vci_instance_not_configured=false\r\nvci_moduletype=J2534\r\nvci_id=\r\n"), 0o644)
}

// restoreVCI puts the user's VCI choice back after an offline session (nothing to do otherwise).
func restoreVCI() {
	path := vciProperties()
	if err := os.Rename(path+".laertes-online", path); err == nil {
		log.Printf("restored the VCI choice in %s", path)
	}
}
