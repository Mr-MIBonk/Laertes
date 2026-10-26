package main

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// The Vehicles tab answers which ODIS project a car needs. It reads ODIS's own vehicle tables from
// the installation chosen in the top bar, so it needs no running ODIS and follows ODIS's updates:
//
//   - VRT_VW-GROUP.xml (vehicle root table): VIN WMI (positions 1-3) → the vehicle project tables
//   - VPT_<BRAND>_<Service|Engineering>_<KWP|UDS>.xml (vehicle project tables): VIN type (positions
//     7-8) and model year (position 10), optionally a PR number that must be fitted → diagnostic
//     projects, plus each project's names in ODIS and communication types
//
// Both come in the vrtvpt plugin JARs (configuration\plugins\de.volkswagen.odis.vaudas.artifact.
// vrtvpt-<brand>_<version>.jar; the newest version of each counts), and ODIS-E may also keep them as
// loose files in VRT_VPT-Engineering. PRNR-INFO.xml in an installed project's folder
// (MCD-Projects\VWMCD\<project>) lists the project's models.

// vehicleDB is what the page gets (GET /api/vehicles). Rules are [type, years, pr, [projects]] with
// the model-year codes a rule covers joined into one string.
type vehicleDB struct {
	Dir      string                     `json:"dir"`
	WMI      map[string][]string        `json:"wmi"`
	Brands   map[string]string          `json:"brands"`
	Tables   map[string][][]any         `json:"tables"`
	Projects map[string]*vehicleProject `json:"projects"`
	Models   [][]string                 `json:"models"` // [project, model project, name, product ID, revision]
}

type vehicleProject struct {
	Names   []string `json:"d"`
	Comm    []string `json:"c,omitempty"`
	Default string   `json:"dc,omitempty"`
	Proto   string   `json:"p,omitempty"`
}

// vrtvptXML covers both table kinds: the root table's idents carry a WMI and table names, the
// project tables' idents a VIN type, model year, PR number and projects.
type vrtvptXML struct {
	XMLName xml.Name
	Brand   string `xml:"BRAND,attr"`
	Name    string `xml:"TABLENAME,attr"`
	Idents  []struct {
		WMI      string   `xml:"VIN-WMI,attr"`
		Tables   []string `xml:"VEHICLEPROJECTTABLE"`
		Type     string   `xml:"VIN-VEHICLE-TYPE,attr"`
		Year     string   `xml:"VIN-MODEL-YEAR,attr"`
		PR       string   `xml:"PR-NUMBER,attr"`
		Projects []string `xml:"DIAGPROJECT"`
	} `xml:"EXPECTEDIDENTSLIST>EXPECTEDIDENTS"`
	Projects []struct {
		Name    string   `xml:"DIAGPROJECT,attr"`
		Names   []string `xml:"DISPLAYNAME"`
		Comm    []string `xml:"BEHAVIOURSPEC>VI-SUFFIX"`
		Default string   `xml:"BEHAVIOURSPEC>DEFAULTCOMMUNICATIONTYPE"`
	} `xml:"PROJECTS>PROJECT"`
}

type prnrXML struct {
	Revision string `xml:"REVISION-DATE"`
	Platform string `xml:"ODX-PLATFORM"`
	Vehicles []struct {
		Project string `xml:"VEHICLE-PROJECT"`
		Name    string `xml:"NAME"`
		ID      string `xml:"PRODUCT-ID"`
	} `xml:"VEHICLES>VEHICLE"`
}

var (
	vehiclesMu    sync.Mutex
	vehiclesCache = map[string]*vehicleDB{} // by the fingerprint of the files read
)

// loadVehicles reads the vehicle tables of the ODIS installation with data folder dir. The result is
// kept until one of the files changes.
func loadVehicles(dir string) (*vehicleDB, error) {
	jars := vrtvptJars(filepath.Join(dir, "configuration", "plugins"))
	loose, _ := filepath.Glob(filepath.Join(dir, "VRT_VPT-Engineering", "*.xml"))
	var prnr []string
	for _, d := range []string{"MCD-Projects", "MCD-Projects-E"} {
		m, _ := filepath.Glob(filepath.Join(dir, d, "VWMCD", "*", "PRNR-INFO.xml"))
		prnr = append(prnr, m...)
	}
	files := slices.Concat(jars, loose, prnr)
	key := dir
	for _, f := range files {
		if st, err := os.Stat(f); err == nil {
			key += fmt.Sprintf("|%s %d %d", f, st.Size(), st.ModTime().UnixNano())
		}
	}
	vehiclesMu.Lock()
	defer vehiclesMu.Unlock()
	if db := vehiclesCache[key]; db != nil {
		return db, nil
	}
	if len(jars)+len(loose) == 0 {
		return nil, fmt.Errorf("no ODIS vehicle tables (vrtvpt plugins) found in %s", dir)
	}

	db := &vehicleDB{Dir: dir, WMI: map[string][]string{}, Brands: map[string]string{},
		Tables: map[string][][]any{}, Projects: map[string]*vehicleProject{}}
	rules := map[string]map[string]int{} // table → rule key → index in db.Tables[table]
	add := func(data []byte) error {
		var t vrtvptXML
		if err := xml.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &t); err != nil {
			return err
		}
		if t.XMLName.Local == "VEHICLE-ROOT-TABLE" {
			for _, id := range t.Idents {
				for _, name := range id.Tables {
					if k := vptKey(name); !slices.Contains(db.WMI[id.WMI], k) {
						db.WMI[id.WMI] = append(db.WMI[id.WMI], k)
					}
				}
			}
			return nil
		}
		table := vptKey(t.Name)
		if t.Brand != "" {
			db.Brands[table] = t.Brand
		}
		if rules[table] == nil {
			rules[table] = map[string]int{}
		}
		for _, id := range t.Idents {
			k := id.Type + "|" + id.PR + "|" + strings.Join(id.Projects, " ")
			i, ok := rules[table][k]
			if !ok {
				i = len(db.Tables[table])
				rules[table][k] = i
				db.Tables[table] = append(db.Tables[table], []any{id.Type, "", id.PR, id.Projects})
			}
			if r := db.Tables[table][i]; !strings.Contains(r[1].(string), id.Year) {
				r[1] = r[1].(string) + id.Year
			}
		}
		for _, p := range t.Projects {
			q := db.Projects[p.Name]
			if q == nil {
				q = &vehicleProject{Names: []string{}}
				db.Projects[p.Name] = q
			}
			for _, n := range p.Names {
				if !slices.Contains(q.Names, n) {
					q.Names = append(q.Names, n)
				}
			}
			if len(p.Comm) > 0 {
				q.Comm = p.Comm
			}
			if p.Default != "" {
				q.Default = p.Default
			}
			if strings.HasSuffix(table, "_UDS") || strings.HasSuffix(table, "_KWP") {
				q.Proto = table[len(table)-3:]
			}
		}
		return nil
	}

	for _, j := range jars {
		zr, err := zip.OpenReader(j)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", j, err)
		}
		for _, f := range zr.File {
			if base := filepath.Base(f.Name); !strings.HasSuffix(base, ".xml") || !(strings.HasPrefix(base, "VRT_") || strings.HasPrefix(base, "VPT_")) {
				continue
			}
			rc, err := f.Open()
			if err == nil {
				var data []byte
				data, err = io.ReadAll(rc)
				rc.Close()
				if err == nil {
					err = add(data)
				}
			}
			if err != nil {
				zr.Close()
				return nil, fmt.Errorf("%s %s: %w", filepath.Base(j), f.Name, err)
			}
		}
		zr.Close()
	}
	for _, f := range loose {
		data, err := os.ReadFile(f)
		if err == nil {
			err = add(data)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
	}
	for _, f := range prnr {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var p prnrXML
		if xml.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &p) != nil {
			continue // a damaged model list only loses its models
		}
		project := p.Platform
		if project == "" {
			project = filepath.Base(filepath.Dir(f))
		}
		for _, v := range p.Vehicles {
			db.Models = append(db.Models, []string{project, v.Project, v.Name, v.ID, p.Revision})
		}
	}
	slices.SortFunc(db.Models, func(a, b []string) int { return strings.Compare(strings.Join(a, "\x00"), strings.Join(b, "\x00")) })
	db.Models = slices.CompactFunc(db.Models, slices.Equal)
	if db.Models == nil {
		db.Models = [][]string{}
	}
	vehiclesCache = map[string]*vehicleDB{key: db}
	return db, nil
}

// vptKey names a vehicle project table without its product, so ODIS-E's and ODIS-S's copies of the
// same rules merge: VPT_VW_Service_UDS → VW_UDS.
func vptKey(name string) string {
	return vptProductRe.ReplaceAllString(strings.TrimPrefix(name, "VPT_"), "_$2")
}

var vptProductRe = regexp.MustCompile(`_(Engineering|Service)_(KWP|UDS)$`)

// vrtvptJars returns the newest version of each vrtvpt plugin JAR in dir; ODIS keeps older ones after
// an update.
func vrtvptJars(dir string) []string {
	all, _ := filepath.Glob(filepath.Join(dir, "de.volkswagen.odis.vaudas.artifact.vrtvpt-*.jar"))
	newest := map[string]string{}
	for _, j := range all {
		name, ver, _ := strings.Cut(strings.TrimSuffix(filepath.Base(j), ".jar"), "_")
		if old, ok := newest[name]; !ok || versionLess(jarVersion(old), ver) {
			newest[name] = j
		}
	}
	out := make([]string, 0, len(newest))
	for _, j := range newest {
		out = append(out, j)
	}
	slices.Sort(out)
	return out
}

func jarVersion(path string) string {
	_, v, _ := strings.Cut(strings.TrimSuffix(filepath.Base(path), ".jar"), "_")
	return v
}

// versionLess compares dotted versions number by number (38.1.40 < 49.0.30).
func versionLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		x, _ := strconv.Atoi(as[i])
		y, _ := strconv.Atoi(bs[i])
		if x != y {
			return x < y
		}
	}
	return len(as) < len(bs)
}
