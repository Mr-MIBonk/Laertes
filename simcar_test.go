package main

import (
	"strings"
	"testing"
)

// A shortened VCDS Auto-Scan in the real layout (made-up VIN and serial numbers).
const scanFixture = `VIN: WVWZZZAUZEW000001   License Plate:

Chassis Type: AU-VW37 (5Q0)
Scan: 03 09 19 5F

VIN: WVWZZZAUZEW000002   Mileage: 128831km-80051miles

-------------------------------------------------------------------------------
Address 03: ABS Brakes (J104)       Labels:. User\5Q0-907-379-IPB-V1.lbl
   Part No SW: 3Q0 907 379 AB    HW: 3Q0 907 379 G
   Component: ESC           H42 0723
   Revision: 00000000    Serial number: 60406000000001
   Coding: 01FA6AA1
   ASAM Dataset: EV_Brake1UDSContiMK100IPB 034036

No fault code found.

-------------------------------------------------------------------------------
Address 09: Cent. Elect. (J519)       Labels:| 5Q0-937-08X-HV1.clb
   Part No SW: 5Q0 937 084 AA    HW: 5Q0 937 084 AA
   Component: BCM MQBAB H   H14 0112
   Serial number: 00011317300001 Dataset Number: V03935237MR 0001
   Coding: 00040B46
   ASAM Dataset: EV_BCMCONTI 011001

   Wiper motor control module:
   Subsystem 1 - Part No SW: 5G1 955 119 A    HW: 5G1 955 119 A  Labels: 5QX-955-119-V1.CLB
   Component: WWS    130613  042 0551
   Coding: 0E4DDD

No fault code found.

-------------------------------------------------------------------------------
Address 19: CAN Gateway (J533)       Labels:| 5Q0-907-530-V1.clb
   Part No SW: 5Q0 907 530 AM    HW: 5Q0 907 530 F
   Component: GW MQB High   125 1265
   Coding: 030100042B
   GVL: 01083C
   ASAM Dataset: EV_GatewLear 013020

1 Fault Found
15360512 - Component Protection
          U1101 00 [00001001] - Active
          Confirmed - Tested Since Memory Clear

-------------------------------------------------------------------------------
Address 5F: Information Electr. (J794)       Labels:* None
   Part No SW: 5NA 000 021 D    HW: 021 D
   Component: MU-H-TNDL-EU053 1367
   Coding: 0273

1 Faults Found
1123600 - Predictive Route Data
          B1630 02 [009] - Signal Failure

End----(Elapsed Time: 01:55)-----
`

func TestParseVCDSScan(t *testing.T) {
	car, err := parseVCDSScan(strings.ReplaceAll(scanFixture, "\n", "\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if car.VIN != "WVWZZZAUZEW000002" {
		t.Errorf("VIN %q: want the one the car reported (under Chassis Type)", car.VIN)
	}
	if len(car.Units) != 4 {
		t.Fatalf("%d units", len(car.Units))
	}
	abs, bcm, gw, mu := car.Units[0], car.Units[1], car.Units[2], car.Units[3]
	if abs.Address != "03" || abs.PartSW != "3Q0907379AB" || abs.PartHW != "3Q0907379G" || abs.System != "ESC" ||
		abs.HWVer != "H42" || abs.SWVer != "0723" || abs.Serial != "60406000000001" || abs.Coding != "01FA6AA1" ||
		abs.ASAM != "EV_Brake1UDSContiMK100IPB" || abs.ASAMVer != "034036" || len(abs.Faults) != 0 {
		t.Errorf("ABS: %+v", abs)
	}
	if bcm.System != "BCM MQBAB H" || bcm.Coding != "00040B46" || bcm.PartSW != "5Q0937084AA" {
		t.Errorf("central electrics (its own lines, not the wiper subsystem): %+v", bcm)
	}
	if len(gw.Faults) != 1 || gw.Faults[0] != (scanFault{Number: 15360512, SAE: "U110100", Status: 0x09, Text: "Component Protection"}) {
		t.Errorf("gateway faults: %+v", gw.Faults)
	}
	if mu.System != "MU-H-TNDL-EU053" || mu.HWVer != "" || mu.SWVer != "1367" || len(mu.Faults) != 1 || mu.Faults[0].Status != 9 {
		t.Errorf("infotainment (one version number, decimal status): %+v", mu)
	}
}

func TestSplitComponent(t *testing.T) {
	for in, want := range map[string][3]string{
		"BCM MQBAB H   H14 0112":  {"BCM MQBAB H", "H14", "0112"},
		"Lenks. Modul  007 0070":  {"Lenks. Modul", "007", "0070"}, // the hardware version also occurs in the software version
		"MU-H-TNDL-EU053 1367":    {"MU-H-TNDL-EU053", "", "1367"},
		"ESC           H42 0723 ": {"ESC", "H42", "0723"},
	} {
		if s, h, w := splitComponent(in); [3]string{s, h, w} != want {
			t.Errorf("%q: %q %q %q", in, s, h, w)
		}
	}
}

func TestSimCarFile(t *testing.T) {
	car, _ := parseVCDSScan(scanFixture)
	ids := canIDs("request_can_id.LL_Brake1UDS  = 713 \nresponse_can_id.LL_Brake1UDS = 77D\n" +
		"request_can_id.LL_GatewUDS  = 710 \nresponse_can_id.LL_GatewUDS = 77A\n" +
		"request_can_id.LL_HighBeamAssisUDS  = 730/748 \nresponse_can_id.LL_HighBeamAssisUDS = 79A/7B2\n")
	if ids["LL_HighBeamAssisUDS"] != [2]string{"730", "79A"} {
		t.Errorf("two ids per link: %v", ids["LL_HighBeamAssisUDS"])
	}
	mapUnits(&car, map[string]string{"03": "Brake1", "09": "CentrElect", "19": "Gatew"}, ids,
		map[string]int{"LL_Brake1UDS": 19, "LL_GatewUDS": 16})
	if car.Units[0].Req != "713" || car.Units[0].Resp != "77D" || car.Units[2].Req != "710" {
		t.Errorf("CAN ids: %+v", car.Units)
	}
	if car.Units[1].Req != "" || len(car.Units[1].Problems) != 1 || len(car.Units[3].Problems) != 1 {
		t.Errorf("09 (no CAN ids) and 5F (not in the project) should be left out: %+v %+v", car.Units[1], car.Units[3])
	}
	f := carFile(car, "test")
	for _, want := range []string{
		"vin WVWZZZAUZEW000002\n",
		"unit 713 77D 03\n",
		"did F187 33 51 30 39 30 37 33 37 39 41 42\n",       // "3Q0907379AB", 11 characters
		"did F197 45 53 43 20 20 20 20 20 20 20 20 20 20\n", // "ESC" padded to 13
		"did 0600 01 FA 6A A1\n",
		"did F1A2 03 40 36\n", // ASAM dataset version "034036": 3 bytes, not text
		// The gateway's component list: slot 0 the gateway (bus master, with its stored fault), the brakes
		// (node 19 = byte 2, bit 3) present, coded as VCDS's GVL says.
		"did 04A3 01 08 3C\n",
		"did 2A26 01 00 08 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00\n",
		"did 2A28 01 00 00 00",
		"did 2A2E 00 01 02 03 04 05 06 07 08 09 0A 0B 0C 0D 0E 0F 10 11 12 13 14", // 256 slots, slot = node
		"did 2A2A 19 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 03 00",
		"did 2A29 01 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 01 00",
		"unit 710 77A 19\n",
		"dtc EA6200 09\n",
	} {
		if !strings.Contains(f, want) {
			t.Errorf("car.txt lacks %q:\n%s", want, f)
		}
	}
	if strings.Contains(f, "unit 70E") || strings.Contains(f, " 5F\n") {
		t.Errorf("car.txt has a unit without CAN ids:\n%s", f)
	}
}
