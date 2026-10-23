package actuators

import (
	"context"
	"testing"
)

func TestDryRunRecordsWithoutSetting(t *testing.T) {
	f := newMem()
	f.vals["s"] = 10
	d := DryRun(f)
	ctx := context.Background()
	if d.Describe().Action != "fake" || d.Check(wl("s")) != "" {
		t.Fatal("describe/check must delegate")
	}
	if v, set, _ := d.Read(ctx, wl("s")); v != 10 || !set {
		t.Fatalf("read %v %v", v, set)
	}
	if err := d.Set(ctx, wl("s"), 4, true); err != nil {
		t.Fatal(err)
	}
	if v, set, _ := d.Read(ctx, wl("s")); v != 4 || !set {
		t.Fatalf("read after set %v %v", v, set)
	}
	if len(f.sets) != 0 || f.vals["s"] != 10 {
		t.Fatal("wrapped actuator was changed")
	}
	d.Set(ctx, wl("s"), 0, false)
	if _, set, _ := d.Read(ctx, wl("s")); set {
		t.Fatal("removal not recorded")
	}
}
