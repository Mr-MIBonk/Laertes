package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

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

// migrate creates or upgrades the schema. Databases from versions that followed ODIS engine.log
// also hold a "readings" table; it is no longer used and is left as it is.
func migrate(db *sql.DB) error {
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
	note      TEXT NOT NULL DEFAULT '',
	vin       TEXT NOT NULL DEFAULT '',
	variant   TEXT NOT NULL DEFAULT ''
);`); err != nil {
		return err
	}
	// Columns added after the table first shipped.
	for _, col := range [][2]string{{"vin", "TEXT NOT NULL DEFAULT ''"}, {"variant", "TEXT NOT NULL DEFAULT ''"},
		{"details", "TEXT NOT NULL DEFAULT ''"}, {"retrofit_id", "INTEGER NOT NULL DEFAULT 0"}} {
		var has int
		if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('changes') WHERE name=?`, col[0]).Scan(&has); err != nil {
			return err
		}
		if has == 0 {
			if _, err := db.Exec(`ALTER TABLE changes ADD COLUMN ` + col[0] + ` ` + col[1]); err != nil {
				return err
			}
		}
	}
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS retrofits (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	name          TEXT NOT NULL,
	vin           TEXT NOT NULL DEFAULT '',
	started       TEXT NOT NULL,
	ended         TEXT NOT NULL DEFAULT '',
	baseline      TEXT NOT NULL DEFAULT '',
	baseline_dtcs TEXT NOT NULL DEFAULT '{}'
);`); err != nil {
		return err
	}
	var hasUnits int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('retrofits') WHERE name='units'`).Scan(&hasUnits); err != nil {
		return err
	}
	if hasUnits == 0 {
		if _, err := db.Exec(`ALTER TABLE retrofits ADD COLUMN units TEXT NOT NULL DEFAULT '[]'`); err != nil {
			return err
		}
	}
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS coding_pref (
	variant        TEXT PRIMARY KEY,
	borrow_variant TEXT NOT NULL,
	borrow_base    TEXT NOT NULL DEFAULT ''
);`); err != nil {
		return err
	}
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL DEFAULT '{}'
);`); err != nil {
		return err
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
	Kind     string `json:"kind"`    // "coding" or "adaptation"
	Address  string `json:"address"` // unit id, e.g. 0x0001
	Unit     string `json:"unit"`    // unit name
	Target   string `json:"target"`  // subsystem (coding) or channel (adaptation)
	OldValue string `json:"oldValue"`
	NewValue string `json:"newValue"`
	Status   string `json:"status"` // verified, written, mismatch, error
	Note     string `json:"note"`
	VIN      string `json:"vin"`     // vehicle VIN, if ODIS knows it
	Variant  string `json:"variant"` // the unit's ODX variant
	// What the change did, one line each: changed bytes and, where a coding definition or
	// labels were known, each setting before -> after.
	Details string `json:"details"`
	// The retrofit that was active when the change was written (0 = none); set by AddChange.
	RetrofitID int64  `json:"retrofitId"`
	Retrofit   string `json:"retrofit"` // its name
}

// AddChange records one written change and sets c.ID. A change written while a retrofit is
// active belongs to that retrofit, if the retrofit covers the changed unit.
func (s *Store) AddChange(c *Change) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	c.RetrofitID, c.Retrofit = 0, ""
	var id int64
	var name, units string
	err = tx.QueryRow(`SELECT id, name, units FROM retrofits WHERE ended='' ORDER BY id DESC LIMIT 1`).Scan(&id, &name, &units)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil {
		var addrs []string
		if err := json.Unmarshal([]byte(units), &addrs); err != nil {
			return err
		}
		if len(addrs) == 0 || slices.Contains(addrs, c.Address) {
			c.RetrofitID, c.Retrofit = id, name
		}
	}
	res, err := tx.Exec(`INSERT INTO changes (time, kind, address, unit, target, old_value, new_value, status, note, vin, variant, details, retrofit_id)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, c.Time, c.Kind, c.Address, c.Unit, c.Target, c.OldValue, c.NewValue, c.Status, c.Note, c.VIN, c.Variant,
		c.Details, c.RetrofitID)
	if err != nil {
		return err
	}
	if c.ID, err = res.LastInsertId(); err != nil {
		return err
	}
	return tx.Commit()
}

// Changes returns the newest changes first; retrofit >= 0 keeps only that retrofit's changes
// (0 = those made outside a retrofit), retrofit < 0 returns all.
func (s *Store) Changes(limit int, retrofit int64) ([]Change, error) {
	rows, err := s.db.Query(`SELECT c.id, c.time, c.kind, c.address, c.unit, c.target, c.old_value, c.new_value, c.status, c.note,
		c.vin, c.variant, c.details, c.retrofit_id, COALESCE(r.name, '')
		FROM changes c LEFT JOIN retrofits r ON r.id = c.retrofit_id WHERE ?<0 OR c.retrofit_id=? ORDER BY c.id DESC LIMIT ?`,
		retrofit, retrofit, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Change{}
	for rows.Next() {
		var c Change
		if err := rows.Scan(&c.ID, &c.Time, &c.Kind, &c.Address, &c.Unit, &c.Target, &c.OldValue, &c.NewValue, &c.Status, &c.Note,
			&c.VIN, &c.Variant, &c.Details, &c.RetrofitID, &c.Retrofit); err != nil {
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

// Retrofit groups the changes made while installing or converting equipment. Starting one
// records a full scan of the car, so faults that were present before can be told apart.
type Retrofit struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	VIN     string `json:"vin"`
	Started string `json:"started"`
	Ended   string `json:"ended"` // empty while the retrofit is active
	// Units are the addresses of the units the retrofit covers; empty means all units.
	Units []string `json:"units"`
	// Baseline is the full-scan log taken at the start (only returned by Retrofit).
	Baseline string `json:"baseline,omitempty"`
	// BaselineDTCs maps a unit address to the faults it had at the start (JSON, kept as sent).
	BaselineDTCs json.RawMessage `json:"baselineDtcs"`
}

var errRetrofitActive = errors.New("a retrofit is already active; end it first")

// StartRetrofit stores r as the active retrofit and sets r.ID; only one can be active.
func (s *Store) StartRetrofit(r *Retrofit) error {
	if len(r.BaselineDTCs) == 0 {
		r.BaselineDTCs = json.RawMessage("{}")
	}
	if r.Units == nil {
		r.Units = []string{}
	}
	units, err := json.Marshal(r.Units)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var active int
	if err := tx.QueryRow(`SELECT count(*) FROM retrofits WHERE ended=''`).Scan(&active); err != nil {
		return err
	}
	if active > 0 {
		return errRetrofitActive
	}
	res, err := tx.Exec(`INSERT INTO retrofits (name, vin, started, baseline, baseline_dtcs, units) VALUES (?,?,?,?,?,?)`,
		r.Name, r.VIN, r.Started, r.Baseline, string(r.BaselineDTCs), string(units))
	if err != nil {
		return err
	}
	if r.ID, err = res.LastInsertId(); err != nil {
		return err
	}
	return tx.Commit()
}

// Retrofits lists all retrofits, newest first, without their baseline logs.
func (s *Store) Retrofits() ([]Retrofit, error) {
	rows, err := s.db.Query(`SELECT id, name, vin, started, ended, baseline_dtcs, units FROM retrofits ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Retrofit{}
	for rows.Next() {
		var r Retrofit
		var dtcs, units string
		if err := rows.Scan(&r.ID, &r.Name, &r.VIN, &r.Started, &r.Ended, &dtcs, &units); err != nil {
			return nil, err
		}
		r.BaselineDTCs = json.RawMessage(dtcs)
		if err := json.Unmarshal([]byte(units), &r.Units); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Retrofit returns one retrofit with its baseline log, or nil if there is none with this id.
func (s *Store) Retrofit(id int64) (*Retrofit, error) {
	r := Retrofit{ID: id}
	var dtcs, units string
	err := s.db.QueryRow(`SELECT name, vin, started, ended, baseline, baseline_dtcs, units FROM retrofits WHERE id=?`, id).
		Scan(&r.Name, &r.VIN, &r.Started, &r.Ended, &r.Baseline, &dtcs, &units)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.BaselineDTCs = json.RawMessage(dtcs)
	if err := json.Unmarshal([]byte(units), &r.Units); err != nil {
		return nil, err
	}
	return &r, nil
}

// UpdateRetrofit renames a retrofit (when name is not empty) and ends it at time ended (when
// ended is not empty and it is still active).
func (s *Store) UpdateRetrofit(id int64, name, ended string) error {
	_, err := s.db.Exec(`UPDATE retrofits SET name=CASE WHEN ?='' THEN name ELSE ? END,
		ended=CASE WHEN ?='' OR ended<>'' THEN ended ELSE ? END WHERE id=?`, name, name, ended, ended, id)
	return err
}

// CodingPref remembers which related ("borrowed") ODX variant decodes a unit variant that has no
// textual coding table of its own, so the choice is reused after restart and on other cars.
type CodingPref struct {
	Variant       string `json:"variant"`
	BorrowVariant string `json:"borrowVariant"`
	BorrowBase    string `json:"borrowBase"`
}

// CodingPref returns the saved preference for variant, or nil if none.
func (s *Store) CodingPref(variant string) (*CodingPref, error) {
	p := CodingPref{Variant: variant}
	err := s.db.QueryRow(`SELECT borrow_variant, borrow_base FROM coding_pref WHERE variant=?`, variant).Scan(&p.BorrowVariant, &p.BorrowBase)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// CodingPrefs returns every saved preference, for the coverage overview.
func (s *Store) CodingPrefs() ([]CodingPref, error) {
	rows, err := s.db.Query(`SELECT variant, borrow_variant, borrow_base FROM coding_pref ORDER BY variant`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CodingPref{}
	for rows.Next() {
		var p CodingPref
		if err := rows.Scan(&p.Variant, &p.BorrowVariant, &p.BorrowBase); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// LabelSet is how many .lbl labels are stored for one variant and subsystem.
type LabelSet struct {
	Variant   string `json:"variant"`
	Subsystem string `json:"subsystem"`
	Count     int    `json:"count"`
}

// LabelSets returns every variant and subsystem that has labels, for the coverage overview.
func (s *Store) LabelSets() ([]LabelSet, error) {
	rows, err := s.db.Query(`SELECT variant, subsystem, count(*) FROM labels GROUP BY variant, subsystem ORDER BY variant, subsystem`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LabelSet{}
	for rows.Next() {
		var l LabelSet
		if err := rows.Scan(&l.Variant, &l.Subsystem, &l.Count); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) SetCodingPref(p CodingPref) error {
	_, err := s.db.Exec(`INSERT INTO coding_pref (variant, borrow_variant, borrow_base) VALUES (?,?,?)
		ON CONFLICT (variant) DO UPDATE SET borrow_variant=excluded.borrow_variant, borrow_base=excluded.borrow_base`,
		p.Variant, p.BorrowVariant, p.BorrowBase)
	return err
}

// Settings returns the stored UI settings as a JSON object ({} when none are saved yet). They are
// kept in the database so they survive restarts and are shared by every browser that opens Laertes.
func (s *Store) Settings() (json.RawMessage, error) { return s.value("ui") }

// SetSettings replaces the stored UI settings with the given JSON object.
func (s *Store) SetSettings(v json.RawMessage) error { return s.setValue("ui", v) }

// CodingRules returns the user's edits of the coding <-> trouble code rules as the page saved them
// ({} when there are none): the page owns their layout and merges them over its built-in rules.
func (s *Store) CodingRules() (json.RawMessage, error) { return s.value("coding_rules") }

// SetCodingRules replaces the stored rule edits with the given JSON object.
func (s *Store) SetCodingRules(v json.RawMessage) error { return s.setValue("coding_rules", v) }

// Coverage returns the last coding coverage check (Coverage tab), {} before the first one.
func (s *Store) Coverage() (json.RawMessage, error) { return s.value("coverage") }

// SetCoverage replaces the stored coverage check.
func (s *Store) SetCoverage(v json.RawMessage) error { return s.setValue("coverage", v) }

// SimCar returns the car the virtual VCI was last given (simcar.go), {} when none.
func (s *Store) SimCar() (json.RawMessage, error) { return s.value("sim_car") }

// SetSimCar replaces it.
func (s *Store) SetSimCar(v json.RawMessage) error { return s.setValue("sim_car", v) }

// value returns the JSON object stored under key in the settings table, {} when there is none.
func (s *Store) value(key string) (json.RawMessage, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return json.RawMessage("{}"), nil
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(v), nil
}

func (s *Store) setValue(key string, v json.RawMessage) error {
	_, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value=excluded.value`, key, string(v))
	return err
}

// Geko is the VW online (GeKo) sign-in kept for ODIS's online functions. In the database the
// password is DPAPI-encrypted for the current Windows user (see protect).
type Geko struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// Geko returns the stored sign-in (empty when none is saved).
func (s *Store) Geko() (Geko, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key='geko'`).Scan(&v)
	if err == sql.ErrNoRows {
		return Geko{}, nil
	}
	if err != nil {
		return Geko{}, err
	}
	var stored struct {
		Username string `json:"username"`
		Password []byte `json:"password"` // encrypted; base64 in the JSON
	}
	if err := json.Unmarshal([]byte(v), &stored); err != nil {
		return Geko{}, err
	}
	pass, err := unprotect(stored.Password)
	if err != nil {
		return Geko{}, fmt.Errorf("decrypt the stored GeKo password (saved under another Windows user?): %w", err)
	}
	return Geko{Username: stored.Username, Password: string(pass)}, nil
}

// SetGeko stores the sign-in, encrypting the password.
func (s *Store) SetGeko(g Geko) error {
	pass, err := protect([]byte(g.Password))
	if err != nil {
		return fmt.Errorf("encrypt the GeKo password: %w", err)
	}
	v, err := json.Marshal(struct {
		Username string `json:"username"`
		Password []byte `json:"password"`
	}{g.Username, pass})
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO settings (key, value) VALUES ('geko', ?)
		ON CONFLICT (key) DO UPDATE SET value=excluded.value`, string(v))
	return err
}

// DeleteCodingPref forgets the saved preference for variant.
func (s *Store) DeleteCodingPref(variant string) error {
	_, err := s.db.Exec(`DELETE FROM coding_pref WHERE variant=?`, variant)
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
