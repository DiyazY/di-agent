package actuators

import (
	"context"
	"errors"
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

// TestDryRunAsksTheActuatorWhetherAWorkloadLeft: with the listing no longer
// deciding that a workload left, a dry run asks the real actuator, so a change
// recorded for a workload that has since left is gone in a dry run too.
func TestDryRunAsksTheActuatorWhetherAWorkloadLeft(t *testing.T) {
	f := newMem()
	d := DryRun(f)
	ctx := context.Background()
	if err := d.Set(ctx, wl("s"), 4, true); err != nil {
		t.Fatal(err)
	}
	f.gone["s"] = true
	if _, _, err := d.Read(ctx, wl("s")); !errors.Is(err, ErrGone) {
		t.Fatalf("Read err %v; want ErrGone", err)
	}
	if err := d.Set(ctx, wl("s"), 0, false); !errors.Is(err, ErrGone) {
		t.Fatalf("Set err %v; want ErrGone", err)
	}
}
