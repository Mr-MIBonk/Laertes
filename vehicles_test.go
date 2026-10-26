package main

import (
	"archive/zip"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeJar(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, body := range files {
		w, _ := zw.Create(name)
		w.Write([]byte(body))
	}
	zw.Close()
	f.Close()
}

// ODIS's vehicle tables, as in the vrtvpt plugin JARs, a loose Engineering table and a project's
// PRNR-INFO.xml, become the Vehicles tab's data; only the newest JAR of a brand counts.
func TestLoadVehicles(t *testing.T) {
	dir := t.TempDir()
	plugins := filepath.Join(dir, "configuration", "plugins")
	os.MkdirAll(plugins, 0o755)
	vrt := "\xef\xbb\xbf" + `<?xml version="1.0" encoding="UTF-8"?><VEHICLE-ROOT-TABLE xmlns="vrtvpt" TABLENAME="VRT_VW-GROUP"><EXPECTEDIDENTSLIST>
		<EXPECTEDIDENTS VIN-WMI="WVW"><VEHICLEPROJECTTABLE>VPT_VW_Service_UDS</VEHICLEPROJECTTABLE></EXPECTEDIDENTS></EXPECTEDIDENTSLIST></VEHICLE-ROOT-TABLE>`
	vpt := `<?xml version="1.0" encoding="UTF-8"?><VEHICLE-PROJECT-TABLE xmlns="vrtvpt" BRAND="VOLKSWAGEN" TABLENAME="VPT_VW_Service_UDS"><EXPECTEDIDENTSLIST>
		<EXPECTEDIDENTS VIN-VEHICLE-TYPE="AU" VIN-MODEL-YEAR="E"><DIAGPROJECT>VW37X</DIAGPROJECT></EXPECTEDIDENTS>
		<EXPECTEDIDENTS VIN-VEHICLE-TYPE="AU" VIN-MODEL-YEAR="F"><DIAGPROJECT>VW37X</DIAGPROJECT></EXPECTEDIDENTS>
		<EXPECTEDIDENTS VIN-VEHICLE-TYPE="CD" VIN-MODEL-YEAR="R" PR-NUMBER="GP1"><DIAGPROJECT>VW38XPA</DIAGPROJECT></EXPECTEDIDENTS>
		</EXPECTEDIDENTSLIST><PROJECTS><PROJECT DIAGPROJECT="VW37X"><DISPLAYNAME>5G - Golf</DISPLAYNAME>
		<BEHAVIOURSPEC><VI-SUFFIX>CAN</VI-SUFFIX><DEFAULTCOMMUNICATIONTYPE>CAN</DEFAULTCOMMUNICATIONTYPE></BEHAVIOURSPEC></PROJECT></PROJECTS></VEHICLE-PROJECT-TABLE>`
	writeJar(t, filepath.Join(plugins, "de.volkswagen.odis.vaudas.artifact.vrtvpt-v_9.0.0.jar"), map[string]string{"artifact/VRT_VW-GROUP.xml": vrt,
		"artifact/VPT_VW_Service_UDS.xml": `<VEHICLE-PROJECT-TABLE TABLENAME="VPT_VW_Service_UDS"><EXPECTEDIDENTSLIST><EXPECTEDIDENTS VIN-VEHICLE-TYPE="XX" VIN-MODEL-YEAR="A"><DIAGPROJECT>OLD</DIAGPROJECT></EXPECTEDIDENTS></EXPECTEDIDENTSLIST></VEHICLE-PROJECT-TABLE>`})
	writeJar(t, filepath.Join(plugins, "de.volkswagen.odis.vaudas.artifact.vrtvpt-v_49.1.0.jar"), map[string]string{"artifact/VRT_VW-GROUP.xml": vrt, "artifact/VPT_VW_Service_UDS.xml": vpt, "plugin.xml": "<plugin/>"})
	eng := filepath.Join(dir, "VRT_VPT-Engineering")
	os.MkdirAll(eng, 0o755)
	os.WriteFile(filepath.Join(eng, "VPT_VW_Engineering_UDS.xml"), []byte(`<VEHICLE-PROJECT-TABLE TABLENAME="VPT_VW_Engineering_UDS"><EXPECTEDIDENTSLIST>
		<EXPECTEDIDENTS VIN-VEHICLE-TYPE="AU" VIN-MODEL-YEAR="G"><DIAGPROJECT>VW37X</DIAGPROJECT></EXPECTEDIDENTS></EXPECTEDIDENTSLIST></VEHICLE-PROJECT-TABLE>`), 0o644)
	proj := filepath.Join(dir, "MCD-Projects-E", "VWMCD", "VW37X")
	os.MkdirAll(proj, 0o755)
	os.WriteFile(filepath.Join(proj, "PRNR-INFO.xml"), []byte(`<PRNR-INFO><REVISION-DATE>2026-04-30T14:54:02</REVISION-DATE><ODX-PLATFORM>VW37X</ODX-PLATFORM>
		<VEHICLES><VEHICLE><VEHICLE-PROJECT>VW37X/0EU_X</VEHICLE-PROJECT><NAME>A7 Golf</NAME><PRODUCT-ID>5G0</PRODUCT-ID></VEHICLE></VEHICLES></PRNR-INFO>`), 0o644)

	db, err := loadVehicles(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]any{{"AU", "EFG", "", []string{"VW37X"}}, {"CD", "R", "GP1", []string{"VW38XPA"}}}
	if !reflect.DeepEqual(db.Tables["VW_UDS"], want) {
		t.Errorf("VW_UDS rules = %v, want %v", db.Tables["VW_UDS"], want)
	}
	if !reflect.DeepEqual(db.WMI["WVW"], []string{"VW_UDS"}) || db.Brands["VW_UDS"] != "VOLKSWAGEN" {
		t.Errorf("WMI %v, brands %v", db.WMI, db.Brands)
	}
	if p := db.Projects["VW37X"]; p == nil || !reflect.DeepEqual(p.Names, []string{"5G - Golf"}) || p.Default != "CAN" || p.Proto != "UDS" {
		t.Errorf("VW37X = %+v", p)
	}
	if !reflect.DeepEqual(db.Models, [][]string{{"VW37X", "VW37X/0EU_X", "A7 Golf", "5G0", "2026-04-30T14:54:02"}}) {
		t.Errorf("models = %v", db.Models)
	}
	if _, err := loadVehicles(t.TempDir()); err == nil {
		t.Error("an empty folder gave no error")
	}
}
