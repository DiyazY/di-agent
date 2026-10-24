package actuators

import (
	"context"
	"testing"

	"github.com/DiyazY/di-agent/pkg/runtime"
)

func fp(v float64) *float64 { return &v }

func TestStaticReadsLimit(t *testing.T) {
	d := Descriptor{Action: "x", Resource: "cpu"}
	a := Static(d, func(w runtime.Workload) string {
		if w.Self {
			return "the agent itself"
		}
		return ""
	}, func(w runtime.Workload) (float64, bool) {
		if l := w.Resources["cpu"].Limit; l != nil {
			return *l, true
		}
		return 0, false
	})
	ctx := context.Background()
	w := runtime.Workload{Subject: "pod:a", Resources: map[string]runtime.Resource{"cpu": {Limit: fp(2)}}}
	if a.Describe().Action != "x" || a.Check(w) != "" {
		t.Fatal("describe/check")
	}
	if a.Check(runtime.Workload{Self: true}) != "the agent itself" {
		t.Fatal("check must use the function given")
	}
	if v, set, err := a.Read(ctx, w); err != nil || !set || v != 2 {
		t.Fatalf("read %v %v %v", v, set, err)
	}
	if err := a.Set(ctx, w, 0.5, true); err != nil {
		t.Fatal(err)
	}
	if v, set, _ := a.Read(ctx, w); !set || v != 0.5 {
		t.Fatalf("read after set %v %v", v, set)
	}
	if v, _, _ := a.Read(ctx, runtime.Workload{Subject: "pod:b", Resources: map[string]runtime.Resource{"cpu": {Limit: fp(3)}}}); v != 3 {
		t.Fatal("another subject must not see the value")
	}
	a.Set(ctx, w, 0, false)
	if _, set, _ := a.Read(ctx, w); set {
		t.Fatal("removal not remembered")
	}
	if a2 := Static(d, nil, func(runtime.Workload) (float64, bool) { return 0, false }); a2.Check(w) != "" {
		t.Fatal("nil check is always actionable")
	}
}
