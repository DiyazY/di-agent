package statemap

import (
	"math"
	"sort"
)

// CalibrationKey names one kind of prediction made on one basis, such as
// ("relief.cpu.limit", "map").
type CalibrationKey struct{ Kind, Basis string }

// Calibration is a running record of one kind of prediction on one basis: how many
// outcomes, the mean signed error (predicted − observed), the mean absolute error,
// and the share of outcomes whose verdict the prediction got right.
type Calibration struct {
	N          int     `json:"n"`
	MeanErr    float64 `json:"mean_err"`
	MeanAbsErr float64 `json:"mean_abs_err"`
	HitRate    float64 `json:"hit_rate"`
}

// CalibrationEntry is the list form, for the snapshot and GET /state.
type CalibrationEntry struct {
	Kind  string `json:"kind"`
	Basis string `json:"basis"`
	Calibration
}

// add folds one outcome in as a running mean, so no history is kept.
func (c *Calibration) add(err float64, hit bool) {
	h := 0.0
	if hit {
		h = 1
	}
	c.N++
	n := float64(c.N)
	c.MeanErr += (err - c.MeanErr) / n
	c.MeanAbsErr += (math.Abs(err) - c.MeanAbsErr) / n
	c.HitRate += (h - c.HitRate) / n
}

// RecordPrediction folds one outcome of a prediction into its calibration: the
// error is predicted − observed, and hit says whether the prediction got the
// verdict right. A prediction or observation that is not a number teaches nothing.
func (m *Map) RecordPrediction(kind, basis string, predicted, observed float64, hit bool) {
	if math.IsNaN(predicted) || math.IsInf(predicted, 0) || math.IsNaN(observed) || math.IsInf(observed, 0) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := CalibrationKey{kind, basis}
	c := m.calibration[k]
	c.add(predicted-observed, hit)
	m.calibration[k] = c
	m.revision++
}

// Calibration is a copy of every calibration record.
func (m *Map) Calibration() map[CalibrationKey]Calibration {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.calibrationLocked()
}

func (m *Map) calibrationLocked() map[CalibrationKey]Calibration {
	out := make(map[CalibrationKey]Calibration, len(m.calibration))
	for k, c := range m.calibration {
		out[k] = c
	}
	return out
}

// CalibrationList is the records sorted by kind then basis; empty, never nil.
func (m *Map) CalibrationList() []CalibrationEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.calibrationListLocked()
}

func (m *Map) calibrationListLocked() []CalibrationEntry {
	out := make([]CalibrationEntry, 0, len(m.calibration))
	for k, c := range m.calibration {
		out = append(out, CalibrationEntry{Kind: k.Kind, Basis: k.Basis, Calibration: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Basis < out[j].Basis
	})
	return out
}
