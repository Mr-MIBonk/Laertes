package main

import (
	"path/filepath"
	"slices"
	"testing"
)

// lblFixture is a made-up label file in the long-coding ("LC") format: notes, bit-field options,
// a single bit, and a line of another type that the parser ignores.
const lblFixture = "LC,00,0=,First general note.\r\n" +
	"LC,00,1=,Second general note.\r\n" +
	"LC,01,0~2,01,Field A: option 1\r\n" +
	"LC,01,0~2,04,Field A: option 4\r\n" +
	"LC,01,3~7,18,Field B: option 0x18\r\n" +
	"LC,03,1~2,02,Field C: option 2\r\n" +
	"LC,03,4,Bit 4 feature\r\n" +
	"SA,12345,Security access line (ignored)\r\n"

func TestParseLBL(t *testing.T) {
	ls, err := parseLBL(lblFixture)
	if err != nil {
		t.Fatal(err)
	}
	if len(ls) != 7 {
		t.Errorf("got %d labels, want 7", len(ls))
	}
	want := map[Label]bool{
		{Byte: -1, Lo: 0, Hi: 0, Value: -1, Text: "First general note."}:   true,
		{Byte: 1, Lo: 3, Hi: 7, Value: 0x18, Text: "Field B: option 0x18"}: true,
		{Byte: 3, Lo: 4, Hi: 4, Value: -1, Text: "Bit 4 feature"}:          true,
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

func TestRetrofitChanges(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	before := Change{Time: "t0", Kind: "coding"}
	s.AddChange(&before)
	rf := Retrofit{Name: "Retrofit VIN1", Started: "t1", Baseline: "scan", BaselineDTCs: []byte(`{"0x0019":["U1113|x"]}`)}
	if err := s.StartRetrofit(&rf); err != nil {
		t.Fatal(err)
	}
	if err := s.StartRetrofit(&Retrofit{Name: "second", Started: "t2"}); err != errRetrofitActive {
		t.Errorf("second active retrofit: %v", err)
	}
	during := Change{Time: "t2", Kind: "coding", Details: "Byte 1: 00 → 04"}
	s.AddChange(&during)
	s.UpdateRetrofit(rf.ID, "Renamed", "t3")
	after := Change{Time: "t4", Kind: "coding"}
	s.AddChange(&after)

	cs, _ := s.Changes(10, -1)
	if len(cs) != 3 || cs[0].RetrofitID != 0 || cs[1].RetrofitID != rf.ID || cs[1].Retrofit != "Renamed" || cs[1].Details != "Byte 1: 00 → 04" || cs[2].RetrofitID != 0 {
		t.Errorf("changes %+v", cs)
	}
	if cs, _ := s.Changes(10, rf.ID); len(cs) != 1 || cs[0].ID != during.ID {
		t.Errorf("retrofit changes %+v", cs)
	}
	if cs, _ := s.Changes(10, 0); len(cs) != 2 {
		t.Errorf("changes outside a retrofit %+v", cs)
	}
	got, _ := s.Retrofit(rf.ID)
	if got == nil || got.Ended != "t3" || got.Baseline != "scan" || string(got.BaselineDTCs) != `{"0x0019":["U1113|x"]}` {
		t.Errorf("retrofit %+v", got)
	}
	next := Retrofit{Name: "next", Started: "t5", Units: []string{"0x0013"}}
	if err := s.StartRetrofit(&next); err != nil {
		t.Errorf("start after end: %v", err)
	}
	acc, other := Change{Time: "t6", Kind: "coding", Address: "0x0013"}, Change{Time: "t7", Kind: "firmware", Address: "0x0019"}
	s.AddChange(&acc)
	s.AddChange(&other)
	if acc.RetrofitID != next.ID || other.RetrofitID != 0 {
		t.Errorf("unit scope: in %d, outside %d", acc.RetrofitID, other.RetrofitID)
	}
	if rs, _ := s.Retrofits(); len(rs) != 2 || !slices.Equal(rs[0].Units, []string{"0x0013"}) || len(rs[1].Units) != 0 {
		t.Errorf("retrofit units %+v", rs)
	}
}

func TestForgetCodingPref(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	s.SetCodingPref(CodingPref{Variant: "EV_A", BorrowVariant: "EV_B"})
	s.SetCodingPref(CodingPref{Variant: "EV_C", BorrowVariant: "EV_D"})
	if err := s.DeleteCodingPref("EV_A"); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.CodingPref("EV_A"); p != nil {
		t.Errorf("EV_A still remembered: %+v", p)
	}
	if p, _ := s.CodingPref("EV_C"); p == nil || p.BorrowVariant != "EV_D" {
		t.Errorf("EV_C lost: %+v", p)
	}
}

func TestSettings(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	v, err := s.Settings()
	if err != nil || string(v) != "{}" {
		t.Fatalf("default settings = %q, %v", v, err)
	}
	if err := s.SetSettings([]byte(`{"dtcCheck":false}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSettings([]byte(`{"dtcCheck":true,"x":1}`)); err != nil { // replaces, not merges
		t.Fatal(err)
	}
	if v, _ := s.Settings(); string(v) != `{"dtcCheck":true,"x":1}` {
		t.Errorf("settings = %q", v)
	}
}
