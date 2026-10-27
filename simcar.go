package main

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// A simulated car for the offline VCI: a VCDS Auto-Scan is turned into car.txt (offlinevci.c
// loadCar), so the VCI answers as that car -- each unit on its own CAN ids with its identification,
// coding and stored faults, and no answer from units the car does not have.

// scanUnit is one control unit of a VCDS Auto-Scan.
type scanUnit struct {
	Address  string      `json:"address"` // "09"
	Name     string      `json:"name"`    // VCDS's name, "Cent. Elect."
	PartSW   string      `json:"partSW"`  // "5Q0937084AA"
	PartHW   string      `json:"partHW"`
	System   string      `json:"system"` // system name, "BCM MQBAB H"
	HWVer    string      `json:"hwVer"`  // "H14"
	SWVer    string      `json:"swVer"`  // "0112"
	Serial   string      `json:"serial"`
	Coding   string      `json:"coding"` // hex
	ASAM     string      `json:"asam"`   // ASAM dataset, "EV_BCMCONTI"
	ASAMVer  string      `json:"asamVer"`
	GVL      string      `json:"gvl,omitempty"` // gateway: its installation list as VCDS prints it (hex)
	Faults   []scanFault `json:"faults"`
	Sname    string      `json:"sname,omitempty"` // ODIS short name of the address in the project
	Req      string      `json:"req,omitempty"`   // CAN request id (hex), "" when not found
	Resp     string      `json:"resp,omitempty"`
	Node     int         `json:"node,omitempty"` // node address in the gateway's component list
	Problems []string    `json:"problems,omitempty"`
}

// scanFault is a stored fault: the VW fault number is the UDS DTC (3 bytes), with its status byte.
type scanFault struct {
	Number int    `json:"number"`
	SAE    string `json:"sae"`
	Status int    `json:"status"`
	Text   string `json:"text"`
}

type scanCar struct {
	VIN   string     `json:"vin"`
	Units []scanUnit `json:"units"`
}

var (
	reAddress   = regexp.MustCompile(`(?m)^Address ([0-9A-Fa-f]{2}): *(.*?)(?: {2,}| *\(|\s*Labels|$)`)
	rePartNo    = regexp.MustCompile(`(?m)^\s*Part No SW: *(.*?) +HW: *(.*?)\s*$`)
	reComponent = regexp.MustCompile(`(?m)^\s*Component: *(.*?)\s*$`)
	reSerial    = regexp.MustCompile(`Serial number: *(\S+)`)
	reCoding    = regexp.MustCompile(`(?m)^\s*Coding: *([0-9A-Fa-f]+)\s*$`)
	reASAM      = regexp.MustCompile(`(?m)^\s*ASAM Dataset: *(\S+) +(\S+)`)
	reGVL       = regexp.MustCompile(`(?m)^\s*GVL: *([0-9A-Fa-f]+)\s*$`)
	reFault     = regexp.MustCompile(`(?m)^\s*(\d{4,8}) - (.*?)\s*\n\s*([PCBU][0-9A-F]{4}) ?([0-9A-F]{2})? *\[([01]{8}|\d{1,3})\]`)
	reCarVIN    = regexp.MustCompile(`(?m)^Chassis Type:[^\n]*\n(?:[^\n]*\n){0,3}?VIN: *([A-HJ-NPR-Z0-9]{17})`)
	reAnyVIN    = regexp.MustCompile(`VIN: *([A-HJ-NPR-Z0-9]{17})`)
)

// parseVCDSScan reads the units of a VCDS Auto-Scan. Subsystems (wiper motor, rain sensor, ...) are
// left out: only a unit's own lines, those before its first "Subsystem", are read.
func parseVCDSScan(text string) (scanCar, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	var car scanCar
	if m := reCarVIN.FindStringSubmatch(text); m != nil { // the VIN the car reported, not the one typed into VCDS
		car.VIN = m[1]
	} else if m := reAnyVIN.FindStringSubmatch(text); m != nil {
		car.VIN = m[1]
	}
	heads := reAddress.FindAllStringSubmatchIndex(text, -1)
	for i, h := range heads {
		end := len(text)
		if i+1 < len(heads) {
			end = heads[i+1][0]
		}
		block := text[h[1]:end]
		own := block
		if k := strings.Index(own, "Subsystem "); k >= 0 {
			own = own[:k]
		}
		u := scanUnit{Address: strings.ToUpper(text[h[2]:h[3]]), Name: strings.TrimSpace(text[h[4]:h[5]])}
		if m := rePartNo.FindStringSubmatch(own); m != nil {
			u.PartSW, u.PartHW = compactPart(m[1]), compactPart(m[2])
		}
		if m := reComponent.FindStringSubmatch(own); m != nil {
			u.System, u.HWVer, u.SWVer = splitComponent(m[1])
		}
		if m := reSerial.FindStringSubmatch(own); m != nil && strings.Trim(m[1], "-") != "" {
			u.Serial = m[1]
		}
		if m := reCoding.FindStringSubmatch(own); m != nil && len(m[1])%2 == 0 {
			u.Coding = strings.ToUpper(m[1])
		}
		if m := reASAM.FindStringSubmatch(own); m != nil {
			u.ASAM, u.ASAMVer = m[1], m[2]
		}
		if m := reGVL.FindStringSubmatch(own); m != nil && len(m[1])%2 == 0 {
			u.GVL = strings.ToUpper(m[1])
		}
		for _, m := range reFault.FindAllStringSubmatch(block, -1) {
			n, _ := strconv.Atoi(m[1])
			f := scanFault{Number: n, SAE: m[3] + m[4], Text: m[2]}
			if len(m[5]) == 8 { // VCDS writes the status as 8 bits, older versions as a decimal number
				s, _ := strconv.ParseInt(m[5], 2, 32)
				f.Status = int(s)
			} else {
				f.Status, _ = strconv.Atoi(m[5])
			}
			if n > 0xFFFFFF {
				u.Problems = append(u.Problems, fmt.Sprintf("fault %d is no UDS fault number: left out", n))
				continue
			}
			u.Faults = append(u.Faults, f)
		}
		car.Units = append(car.Units, u)
	}
	if len(car.Units) == 0 {
		return car, fmt.Errorf("no \"Address XX:\" unit found: paste a whole VCDS Auto-Scan")
	}
	return car, nil
}

// compactPart writes a part number as the unit reports it: "04E 906 016 F" -> "04E906016F".
func compactPart(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), " ", "")
	if strings.Trim(s, "-") == "" {
		return ""
	}
	return s
}

// splitComponent splits VCDS's component line "BCM MQBAB H   H14 0112" into the system name, the
// hardware version and the software version; a line with one number has only the software version.
func splitComponent(s string) (system, hw, sw string) {
	s = strings.TrimSpace(s)
	if m := reComponentHWSW.FindStringSubmatch(s); m != nil {
		return m[1], m[2], m[3]
	}
	if m := reComponentSW.FindStringSubmatch(s); m != nil {
		return m[1], "", m[2]
	}
	return s, "", ""
}

var (
	reComponentHWSW = regexp.MustCompile(`^(.*\S)\s+(\S{3})\s+(\S{4})$`)
	reComponentSW   = regexp.MustCompile(`^(.*\S)\s+(\S{4})$`)
)

var reCANID = regexp.MustCompile(`(?m)^\s*(request|response)_can_id\.(LL_\S+)\s*=\s*([0-9A-Fa-f]+)`)

// canIDs reads ODIS's can_ids.properties: logical link -> [request, response] CAN id (hex). Where a
// link lists two ids ("730/748") the first is used.
func canIDs(props string) map[string][2]string {
	out := map[string][2]string{}
	for _, m := range reCANID.FindAllStringSubmatch(props, -1) {
		v := out[m[2]]
		if m[1] == "request" {
			v[0] = strings.ToUpper(m[3])
		} else {
			v[1] = strings.ToUpper(m[3])
		}
		out[m[2]] = v
	}
	return out
}

// mapUnits gives each unit its CAN ids and node address: its address names an ODIS short name in the
// vehicle project (snames, from the page's unit list), whose logical link LL_<sname>UDS is in
// can_ids.properties and in the gateway's node table (nodes, from the bridge's gateway_nodes).
func mapUnits(car *scanCar, snames map[string]string, ids map[string][2]string, nodes map[string]int) {
	for i := range car.Units {
		u := &car.Units[i]
		u.Sname = snames[u.Address]
		if u.Sname == "" {
			u.Problems = append(u.Problems, "the vehicle project has no unit at this address: left out")
			continue
		}
		id, ok := ids["LL_"+u.Sname+"UDS"]
		if !ok || id[0] == "" || id[1] == "" {
			u.Problems = append(u.Problems, "no CAN ids for LL_"+u.Sname+"UDS in ODIS's can_ids.properties: left out")
			continue
		}
		u.Req, u.Resp = id[0], id[1]
		u.Node = nodes["LL_"+u.Sname+"UDS"]
	}
}

// gatewayLists is the simulated gateway's component list, the DIDs of TAB_RecorDataIdentGatewCompoList
// in ODIS's gateway ODX, which a Quick scan reads to learn which units the car has. The bit lists
// hold one bit per node address (bit 0 of the first byte is node 0), 32 bytes for nodes 0..255; VCDS
// prints the coded list as the gateway's "GVL", which is used as it is. The other lists hold one
// entry per unit, in the order of the node address list (2A2E).
func gatewayLists(car scanCar, gw scanUnit) map[string][]byte {
	var units []scanUnit
	for _, u := range car.Units {
		if u.Req != "" && u.Node > 0 && u.Node < 256 && u.Address != gw.Address {
			units = append(units, u)
		}
	}
	sort.Slice(units, func(i, j int) bool { return units[i].Node < units[j].Node })
	bits := func(set func(scanUnit) bool) []byte {
		b := make([]byte, 32)
		for _, u := range units {
			if set(u) {
				b[u.Node/8] |= 1 << (u.Node % 8)
			}
		}
		return b
	}
	// Slot 0 is the gateway itself as bus master (bit 0 of a real gateway's coded list is set).
	present := bits(func(scanUnit) bool { return true })
	present[0] |= 1
	coded, err := hex.DecodeString(gw.GVL)
	if err != nil || len(coded) == 0 {
		coded = append([]byte(nil), present...)
	}
	dtcs := bits(func(u scanUnit) bool { return len(u.Faults) > 0 })
	if len(gw.Faults) > 0 {
		dtcs[0] |= 1
	}
	// Every list has the same 256 slots, one per node address: ODIS's job turns slot i of each list
	// into one entry and fails when a byte list is shorter than the bit lists.
	nodes, prot, bus, ids := make([]byte, 256), make([]byte, 256), make([]byte, 256), make([]byte, 256)
	for i := range nodes {
		nodes[i] = byte(i)
	}
	prot[0], ids[0] = 0x01, 0x19 // the gateway: ISO-TP, bus master (bus type 0)
	for _, u := range units {
		addr, _ := strconv.ParseUint(u.Address, 16, 8)
		prot[u.Node] = 0x01 // diagnostic protocol: ISO-TP
		bus[u.Node] = 0x10  // data bus: CAN
		ids[u.Node] = byte(addr)
	}
	return map[string][]byte{
		"04A3": coded,            // coded
		"2A26": present,          // present
		"2A27": make([]byte, 32), // asleep: none
		"2A28": dtcs,             // with stored faults
		"2A29": prot,             // diagnostic protocol
		"2A2A": ids,              // allocation: the unit's diagnostic address
		"2A2D": bus,              // data bus
		"2A2E": nodes,            // node addresses
		"2EE0": make([]byte, 32), // diagnosis path
		"2EE1": make([]byte, 32), // authentication
	}
}

// carFile writes car.txt (format in offlinevci.c loadCar) for the units that have CAN ids.
func carFile(car scanCar, source string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Simulated car for Laertes's virtual VCI, from %s. Delete this file to get the generic unit back.\n", source)
	if car.VIN != "" {
		fmt.Fprintf(&b, "vin %s\n", car.VIN)
	}
	units := append([]scanUnit(nil), car.Units...)
	sort.Slice(units, func(i, j int) bool { return units[i].Address < units[j].Address })
	for _, u := range units {
		if u.Req == "" {
			continue
		}
		fmt.Fprintf(&b, "\n# %s %s (%s)\nunit %s %s %s\n", u.Address, u.Name, u.Sname, u.Req, u.Resp, u.Address)
		ascii := func(did string, v string, width int) {
			if v == "" {
				return
			}
			if width > len(v) {
				v += strings.Repeat(" ", width-len(v))
			}
			fmt.Fprintf(&b, "did %s %s\n", did, spacedHex([]byte(v)))
		}
		ascii("F187", u.PartSW, 11) // VW spare part number
		ascii("F189", u.SWVer, 4)   // software version
		ascii("F191", u.PartHW, 11) // hardware number
		ascii("F1A3", u.HWVer, 3)   // hardware version
		ascii("F197", u.System, 13) // system name
		ascii("F18C", u.Serial, 0)  // serial number
		ascii("F19E", u.ASAM, 0)    // ASAM/ODX dataset
		// Its version is 3 bytes, which VCDS prints as 6 hex digits ("013020" = 01 30 20). With F19E it
		// is what ODIS identifies the variant by.
		if v, err := hex.DecodeString(u.ASAMVer); err == nil && len(v) == 3 {
			fmt.Fprintf(&b, "did F1A2 %s\n", spacedHex(v))
		}
		if c, err := hex.DecodeString(u.Coding); err == nil && len(c) > 0 && len(c) <= 64 {
			fmt.Fprintf(&b, "did 0600 %s\n", spacedHex(c))
		}
		if u.Address == "19" {
			lists := gatewayLists(car, u)
			dids := make([]string, 0, len(lists))
			for did := range lists {
				dids = append(dids, did)
			}
			sort.Strings(dids)
			fmt.Fprintf(&b, "# component list (Quick scan)\n")
			for _, did := range dids {
				if len(lists[did]) > 0 {
					fmt.Fprintf(&b, "did %s %s\n", did, spacedHex(lists[did]))
				}
			}
		}
		for _, f := range u.Faults {
			fmt.Fprintf(&b, "dtc %06X %02X\n", f.Number, f.Status)
		}
	}
	return b.String()
}

func spacedHex(b []byte) string {
	s := make([]string, len(b))
	for i, c := range b {
		s[i] = fmt.Sprintf("%02X", c)
	}
	return strings.Join(s, " ")
}

// simCarPath is car.txt next to the VCI DLL (in the DLL's folder, like offline.conf).
func simCarPath() string { return filepath.Join(filepath.Dir(offlineConfPath), "car.txt") }

// canIDsPath is ODIS's table of CAN ids per logical link.
func canIDsPath() string { return filepath.Join(odisDir, "can_ids.properties") }

// setSimCar parses scan, maps its units to CAN ids and writes car.txt; the VCI takes it on its next
// request. It returns the units with what was found or left out.
func setSimCar(scan string, snames map[string]string, nodes map[string]int) (scanCar, error) {
	car, err := parseVCDSScan(scan)
	if err != nil {
		return car, err
	}
	props, err := os.ReadFile(canIDsPath())
	if err != nil {
		return car, fmt.Errorf("ODIS's CAN id table is missing (%v): is ODIS %s installed?", err, odisProduct)
	}
	mapUnits(&car, snames, canIDs(string(props)), nodes)
	n := 0
	for _, u := range car.Units {
		if u.Req != "" {
			n++
		}
	}
	if n == 0 {
		return car, fmt.Errorf("none of the scan's units has CAN ids in this vehicle project")
	}
	if err := os.MkdirAll(filepath.Dir(simCarPath()), 0o755); err != nil {
		return car, err
	}
	tmp := simCarPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(carFile(car, "a VCDS Auto-Scan of "+car.VIN)), 0o644); err != nil {
		return car, err
	}
	return car, os.Rename(tmp, simCarPath())
}

// clearSimCar deletes car.txt: the VCI answers as the generic unit again.
func clearSimCar() error {
	if err := os.Remove(simCarPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
