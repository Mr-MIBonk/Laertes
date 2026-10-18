package main

import (
	"database/sql"

	_ "modernc.org/sqlite"
)

// Label follows the VCDS long-coding model:
//   - Lo == Hi, Value == -1: meaning of a single bit when it is set
//   - Value >= 0: one named option of the bit field Lo..Hi (Value is byte & mask)
//   - Lo == 0, Hi == 7, Value == -1: description of the whole byte
//   - Byte == -1: general note about the control unit, ordered by Lo
type Label struct {
	Byte  int    `json:"byte"`
	Lo    int    `json:"lo"`
	Hi    int    `json:"hi"`
	Value int    `json:"value"`
	Text  string `json:"text"`
}

func (l Label) valid() bool {
	if l.Byte == -1 {
		return l.Lo >= 0 && l.Value == -1
	}
	return l.Byte >= 0 && l.Lo >= 0 && l.Lo <= l.Hi && l.Hi <= 7 && l.Value >= -1 && l.Value <= 255
}

type Store struct{ db *sql.DB }

func openStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		return nil, err
	}
	return &Store{db}, nil
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS readings (
	id        INTEGER PRIMARY KEY AUTOINCREMENT,
	time      TEXT NOT NULL,
	ecu       TEXT NOT NULL DEFAULT '',
	variant   TEXT NOT NULL,
	subsystem TEXT NOT NULL,
	coding    TEXT NOT NULL,
	UNIQUE (time, variant, subsystem, coding)
);`)
	if err != nil {
		return err
	}
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS changes (
	id        INTEGER PRIMARY KEY AUTOINCREMENT,
	time      TEXT NOT NULL,
	kind      TEXT NOT NULL,
	address   TEXT NOT NULL DEFAULT '',
	unit      TEXT NOT NULL DEFAULT '',
	target    TEXT NOT NULL DEFAULT '',
	old_value TEXT NOT NULL DEFAULT '',
	new_value TEXT NOT NULL DEFAULT '',
	status    TEXT NOT NULL DEFAULT '',
	note      TEXT NOT NULL DEFAULT ''
);`); err != nil {
		return err
	}
	// Deleted readings stay as hidden rows so re-reading the log does not bring them back.
	var hasDeleted int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('readings') WHERE name='deleted'`).Scan(&hasDeleted); err != nil {
		return err
	}
	if hasDeleted == 0 {
		if _, err := db.Exec(`ALTER TABLE readings ADD COLUMN deleted INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	// Version 1 stored per-bit labels in "labels" with a "bit" column (-1 = whole byte).
	var oldSchema int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('labels') WHERE name='bit'`).Scan(&oldSchema); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if oldSchema > 0 {
		if _, err := tx.Exec(`ALTER TABLE labels RENAME TO labels_v1`); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS labels (
	variant   TEXT NOT NULL,
	subsystem TEXT NOT NULL,
	byte      INTEGER NOT NULL,
	lo        INTEGER NOT NULL,
	hi        INTEGER NOT NULL,
	value     INTEGER NOT NULL,
	text      TEXT NOT NULL,
	PRIMARY KEY (variant, subsystem, byte, lo, hi, value)
);`); err != nil {
		return err
	}
	if oldSchema > 0 {
		if _, err := tx.Exec(`
INSERT INTO labels (variant, subsystem, byte, lo, hi, value, text)
SELECT variant, subsystem, byte, CASE WHEN bit < 0 THEN 0 ELSE bit END, CASE WHEN bit < 0 THEN 7 ELSE bit END, -1, text FROM labels_v1;
DROP TABLE labels_v1;`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AddReading stores r and sets r.ID; re-reading the same log line is a no-op.
func (s *Store) AddReading(r *Reading) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO readings (time, ecu, variant, subsystem, coding) VALUES (?,?,?,?,?)`,
		r.Time, r.ECU, r.Variant, r.Subsystem, r.Coding)
	if err != nil {
		return err
	}
	return s.db.QueryRow(`SELECT id FROM readings WHERE time=? AND variant=? AND subsystem=? AND coding=?`,
		r.Time, r.Variant, r.Subsystem, r.Coding).Scan(&r.ID)
}

func (s *Store) SetReadingECU(r *Reading) error {
	_, err := s.db.Exec(`UPDATE readings SET ecu=? WHERE id=?`, r.ECU, r.ID)
	return err
}

// DeleteReadings hides one reading, or all of them when id is 0.
func (s *Store) DeleteReadings(id int64) error {
	_, err := s.db.Exec(`UPDATE readings SET deleted=1 WHERE id=? OR ?=0`, id, id)
	return err
}

func (s *Store) Readings(limit int) ([]Reading, error) {
	rows, err := s.db.Query(`SELECT id, time, ecu, variant, subsystem, coding FROM readings WHERE deleted=0 ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Reading{}
	for rows.Next() {
		var r Reading
		if err := rows.Scan(&r.ID, &r.Time, &r.ECU, &r.Variant, &r.Subsystem, &r.Coding); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) Labels(variant, subsystem string) ([]Label, error) {
	rows, err := s.db.Query(`SELECT byte, lo, hi, value, text FROM labels WHERE variant=? AND subsystem=? ORDER BY byte, lo, hi, value`, variant, subsystem)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Label{}
	for rows.Next() {
		var l Label
		if err := rows.Scan(&l.Byte, &l.Lo, &l.Hi, &l.Value, &l.Text); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// Change is one coding or adaptation value the tool wrote to a control unit.
type Change struct {
	ID       int64  `json:"id"`
	Time     string `json:"time"`
	Kind     string `json:"kind"`     // "coding" or "adaptation"
	Address  string `json:"address"`  // unit id, e.g. 0x0001
	Unit     string `json:"unit"`     // unit name
	Target   string `json:"target"`   // subsystem (coding) or channel (adaptation)
	OldValue string `json:"oldValue"`
	NewValue string `json:"newValue"`
	Status   string `json:"status"` // verified, written, mismatch, error
	Note     string `json:"note"`
}

// AddChange records one written change and sets c.ID.
func (s *Store) AddChange(c *Change) error {
	res, err := s.db.Exec(`INSERT INTO changes (time, kind, address, unit, target, old_value, new_value, status, note)
		VALUES (?,?,?,?,?,?,?,?,?)`, c.Time, c.Kind, c.Address, c.Unit, c.Target, c.OldValue, c.NewValue, c.Status, c.Note)
	if err != nil {
		return err
	}
	c.ID, err = res.LastInsertId()
	return err
}

func (s *Store) Changes(limit int) ([]Change, error) {
	rows, err := s.db.Query(`SELECT id, time, kind, address, unit, target, old_value, new_value, status, note
		FROM changes ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Change{}
	for rows.Next() {
		var c Change
		if err := rows.Scan(&c.ID, &c.Time, &c.Kind, &c.Address, &c.Unit, &c.Target, &c.OldValue, &c.NewValue, &c.Status, &c.Note); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteChanges removes one change, or all of them when id is 0.
func (s *Store) DeleteChanges(id int64) error {
	_, err := s.db.Exec(`DELETE FROM changes WHERE id=? OR ?=0`, id, id)
	return err
}

// SetLabels saves labels in one transaction; an empty text removes that label.
func (s *Store) SetLabels(variant, subsystem string, ls []Label) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, l := range ls {
		if l.Text == "" {
			_, err = tx.Exec(`DELETE FROM labels WHERE variant=? AND subsystem=? AND byte=? AND lo=? AND hi=? AND value=?`,
				variant, subsystem, l.Byte, l.Lo, l.Hi, l.Value)
		} else {
			_, err = tx.Exec(`INSERT INTO labels (variant, subsystem, byte, lo, hi, value, text) VALUES (?,?,?,?,?,?,?)
				ON CONFLICT (variant, subsystem, byte, lo, hi, value) DO UPDATE SET text=excluded.text`,
				variant, subsystem, l.Byte, l.Lo, l.Hi, l.Value, l.Text)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
