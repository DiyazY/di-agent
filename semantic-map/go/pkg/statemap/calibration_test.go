package statemap

import (
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestCalibrationIsAWelfordRunningMean(t *testing.T) {
	m, _ := newTestMap(t, Config{})
	k, b := "relief.cpu.limit", "map"
	m.RecordPrediction(k, b, 0.10, 0.06, true)
	m.RecordPrediction(k, b, 0.10, 0.14, false)
	m.RecordPrediction(k, b, 0.20, 0.10, true)

	c := m.Calibration()[CalibrationKey{k, b}]
	if c.N != 3 || !near(c.MeanErr, 0.1/3) || !near(c.MeanAbsErr, 0.06) || !near(c.HitRate, 2.0/3) {
		t.Fatalf("calibration %+v", c)
	}
	if got := m.Calibration()[CalibrationKey{k, "usage"}]; got != (Calibration{}) {
		t.Fatalf("an unrecorded basis is %+v; want the zero value", got)
	}
	if len(m.Calibration()) != 1 {
		t.Fatalf("calibration %v; want one key", m.Calibration())
	}
}

func TestRecordPredictionIgnoresNonFinite(t *testing.T) {
	m, _ := newTestMap(t, Config{})
	m.RecordPrediction("k", "b", 0.1, 0.1, true)
	rev := m.Revision()
	m.RecordPrediction("k", "b", math.NaN(), 0.1, true)
	m.RecordPrediction("k", "b", 0.1, math.Inf(1), true)
	if n := m.Calibration()[CalibrationKey{"k", "b"}].N; n != 1 || m.Revision() != rev {
		t.Fatalf("N %d revision %d -> %d; want unchanged", n, rev, m.Revision())
	}
}

func TestCalibrationListIsSortedAndCopied(t *testing.T) {
	m, _ := newTestMap(t, Config{})
	if l := m.CalibrationList(); l == nil || len(l) != 0 {
		t.Fatalf("empty list is %#v; want non-nil and empty", l)
	}
	m.RecordPrediction("relief.memory.high", "usage", 0.1, 0.1, true)
	m.RecordPrediction("admission.cpu_contention_ratio", "usage", 0.1, 0.1, true)
	m.RecordPrediction("admission.cpu_contention_ratio", "map", 0.1, 0.1, true)
	var got []string
	for _, e := range m.CalibrationList() {
		got = append(got, e.Kind+"/"+e.Basis)
	}
	want := []string{"admission.cpu_contention_ratio/map", "admission.cpu_contention_ratio/usage", "relief.memory.high/usage"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order %v; want %v", got, want)
	}

	cp := m.Calibration()
	cp[CalibrationKey{"relief.memory.high", "usage"}] = Calibration{N: 99}
	if m.Calibration()[CalibrationKey{"relief.memory.high", "usage"}].N != 1 {
		t.Fatal("mutating the returned map changed the map's own record")
	}
}

func TestCalibrationSurvivesRestart(t *testing.T) {
	path := t.TempDir() + "/state.json"
	m1, _ := newTestMap(t, Config{})
	m1.RecordPrediction("relief.cpu.limit", "map", 0.1, 0.06, true)
	m1.RecordPrediction("relief.cpu.limit", "map", 0.1, 0.2, false)
	if err := m1.Save(path); err != nil {
		t.Fatal(err)
	}
	m2, _ := newTestMap(t, Config{})
	if r, err := m2.Load(path); err != nil || !r.Loaded {
		t.Fatalf("load: %v %v", r.Loaded, err)
	}
	if !reflect.DeepEqual(m1.Calibration(), m2.Calibration()) || m2.Calibration()[CalibrationKey{"relief.cpu.limit", "map"}].N != 2 {
		t.Fatalf("restored %v; had %v", m2.Calibration(), m1.Calibration())
	}

	blob, _ := os.ReadFile(path)
	old := strings.Replace(string(blob), `"version":8`, `"version":7`, 1)
	if old == string(blob) {
		t.Fatal("snapshot does not carry version 8")
	}
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	m3, _ := newTestMap(t, Config{})
	if _, err := m3.Load(path); err == nil {
		t.Fatal("a version 7 snapshot was accepted")
	}
}

func TestStateViewCarriesCalibration(t *testing.T) {
	m, _ := newTestMap(t, Config{})
	if c := m.State(Query{}).Calibration; c == nil || len(c) != 0 {
		t.Fatalf("fresh view calibration %#v; want non-nil and empty", c)
	}
	m.RecordPrediction("k", "b", 0.1, 0.1, true)
	if c := m.State(Query{}).Calibration; len(c) != 1 || c[0].Kind != "k" || c[0].N != 1 {
		t.Fatalf("view calibration %+v", c)
	}
}
