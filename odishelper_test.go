package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLogParser(t *testing.T) {
	var got []*Reading
	p := &logParser{emit: func(r *Reading) { got = append(got, r) }, update: func(*Reading) {}}
	for _, l := range []string{
		`20261018 09:41:33:233 [INFO ] [Job-Thread-195] de.volkswagen.ecf.services.EcuComMcdService.parseMCDResult(null:-1) -> ECU-Variant = <GV_ECM10TFS011VW37X_231>`,
		`20261018 09:41:33:241 [INFO ] [ModalContext] de.volkswagen.odis.vaudas.diag.zecret.ecuonboard.uds.coding.CodingUds.getByteCodingValue(null:-1) -> Output: subsystem <IdentifiedSystem(name=R3 1.0l TFS  ,index=-1)>, coding value <0000: 01 19 00 32 23 24 05 08 00 00                       ...2#$....>, the length of the byte-array is <10>.`,
		`20261018 09:41:33:245 [INFO ] [ModalContext] de.volkswagen.odis.vaudas.ui.selfdiag.pub.AbstractSelfdiagFunction.logResultModelCreation(null:-1) -> Function <SelfdiagFunction.coding.name> was called on control unit <Engine electronics>.`,
		// Multi-line dump for a coding longer than 16 bytes.
		`20261018 10:00:00:000 [INFO ] [ModalContext] de.volkswagen.odis.vaudas.diag.zecret.ecuonboard.uds.coding.CodingUds.getByteCodingValue(null:-1) -> Output: subsystem <IdentifiedSystem(name=GW ,index=2)>, coding value <0000: 00 01 02 03 04 05 06 07 08 09 0A 0B 0C 0D 0E 0F  ................`,
		`0010: 41 42                                            AB>, the length of the byte-array is <18>.`,
	} {
		p.line(l)
	}
	if len(got) != 2 {
		t.Fatalf("got %d readings", len(got))
	}
	if r := got[0]; r.Coding != "01190032232405080000" || r.Variant != "GV_ECM10TFS011VW37X_231" || r.Subsystem != "R3 1.0l TFS" || r.ECU != "Engine electronics" || r.Time != "20261018 09:41:33:241" {
		t.Errorf("first reading %+v", r)
	}
	if got[1].Coding != "000102030405060708090A0B0C0D0E0F4142" {
		t.Errorf("multi-line coding %s", got[1].Coding)
	}
}

func TestParseLBL(t *testing.T) {
	src, err := os.ReadFile("04C-906-020-CHY.lbl")
	if err != nil {
		t.Skip(err)
	}
	ls, err := parseLBL(latin1(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(ls) != 20 {
		t.Errorf("got %d labels, want 20", len(ls))
	}
	want := map[Label]bool{
		{Byte: -1, Lo: 0, Hi: 0, Value: -1, Text: "Only Limited Coding Information is available for this Control Module."}: true,
		{Byte: 1, Lo: 3, Hi: 7, Value: 0x18, Text: "Transmission Gear Ratio: 6-Speed"}:                                     true,
		{Byte: 3, Lo: 4, Hi: 4, Value: -1, Text: "Airbags installed"}:                                                      true,
	}
	for _, l := range ls {
		delete(want, l)
	}
	if len(want) > 0 {
		t.Errorf("missing %+v", want)
	}
}

func TestMigrateV1Labels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")
	s, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s.db.Exec(`DROP TABLE labels`)
	s.db.Exec(`CREATE TABLE labels (variant TEXT, subsystem TEXT, byte INTEGER, bit INTEGER, text TEXT)`)
	s.db.Exec(`INSERT INTO labels VALUES ('v','s',2,-1,'whole'), ('v','s',2,5,'b5')`)
	s.db.Close()
	if s, err = openStore(path); err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	ls, _ := s.Labels("v", "s")
	if len(ls) != 2 || ls[0] != (Label{2, 0, 7, -1, "whole"}) || ls[1] != (Label{2, 5, 5, -1, "b5"}) {
		t.Errorf("migrated %+v", ls)
	}
}

func TestDeletedReadingStaysDeleted(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	r := &Reading{Time: "20261018 09:41:33:241", Variant: "v", Subsystem: "s", Coding: "0119"}
	s.AddReading(r)
	s.AddReading(&Reading{Time: "20261018 09:50:00:000", Variant: "v", Subsystem: "s", Coding: "0219"})
	if err := s.DeleteReadings(r.ID); err != nil {
		t.Fatal(err)
	}
	s.AddReading(&Reading{Time: r.Time, Variant: "v", Subsystem: "s", Coding: "0119"}) // log re-read after restart
	if rs, _ := s.Readings(10); len(rs) != 1 || rs[0].Coding != "0219" {
		t.Errorf("after delete: %+v", rs)
	}
	s.DeleteReadings(0)
	if rs, _ := s.Readings(10); len(rs) != 0 {
		t.Errorf("after clear all: %+v", rs)
	}
}
