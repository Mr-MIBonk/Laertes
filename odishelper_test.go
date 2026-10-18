package main

import (
	"path/filepath"
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
