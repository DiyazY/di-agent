package runtime

import (
	"context"
	"strings"
	"testing"
)

func f64(v float64) *float64 { return &v }

func newDry(t *testing.T, node string, subjects ...string) *DryRun {
	t.Helper()
	r, err := Build("dry-run", Env{NodeID: node, Subjects: func() []string { return subjects }})
	if err != nil {
		t.Fatal(err)
	}
	return r.(*DryRun)
}

func TestWorkloadsAreCopies(t *testing.T) {
	d := newDry(t, "n1")
	d.Put(Workload{Subject: "pod:a", Name: "a", Labels: map[string]string{"k": "v"},
		Resources: map[string]Resource{"cpu": {Limit: f64(1)}}})
	ws, _ := d.Workloads(context.Background())
	*ws[0].Resources["cpu"].Limit = 9
	ws[0].Labels["k"] = "x"
	ws[0].Resources["memory"] = Resource{}
	again, _ := d.Workloads(context.Background())
	if *again[0].Resources["cpu"].Limit != 1 || again[0].Labels["k"] != "v" {
		t.Fatalf("state leaked: %+v", again[0])
	}
	if _, ok := again[0].Resources["memory"]; ok {
		t.Fatal("Resources leaked")
	}
}

func TestAnUnknownRuntimeNamesTheKnown(t *testing.T) {
	_, err := Build("nope", Env{})
	if err == nil || !strings.Contains(err.Error(), "dry-run") {
		t.Fatalf("err %v", err)
	}
}

func TestTheMapsSubjectsBecomeWorkloads(t *testing.T) {
	d := newDry(t, "n1", "pod:a")
	ws, err := d.Workloads(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 1 || ws[0].Subject != "pod:a" || ws[0].Name != "pod:a" || ws[0].Resources != nil {
		t.Fatalf("ws %+v", ws)
	}
}

func TestARemovedWorkloadIsNotReadmitted(t *testing.T) {
	d := newDry(t, "n1", "pod:a")
	if ws, _ := d.Workloads(context.Background()); len(ws) != 1 {
		t.Fatalf("ws %+v", ws)
	}
	d.Remove("pod:a")
	if ws, _ := d.Workloads(context.Background()); len(ws) != 0 {
		t.Fatalf("ws %+v; a removed workload came back from the map's subjects", ws)
	}
}
