package actuators

import (
	"context"
	"strings"
	"testing"

	"github.com/DiyazY/di-agent/pkg/runtime"
)

type fake struct{ d Descriptor }

func (f fake) Describe() Descriptor          { return f.d }
func (f fake) Check(runtime.Workload) string { return "" }
func (f fake) Read(context.Context, runtime.Workload) (float64, bool, error) {
	return 0, false, nil
}
func (f fake) Set(context.Context, runtime.Workload, float64, bool) error { return nil }

func fakeFactory(action string) Factory {
	return func(Env) (Actuator, error) { return fake{Descriptor{Action: action}}, nil }
}

func mustPanic(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s: expected panic", name)
		}
	}()
	fn()
}

func TestRegisterPanicsOnEmptyOrDuplicate(t *testing.T) {
	resetForTest()
	mustPanic(t, "empty", func() { Register("", fakeFactory("")) })
	Register("a", fakeFactory("a"))
	mustPanic(t, "duplicate", func() { Register("a", fakeFactory("a")) })
}

func TestBuildUnknownNamesRegistered(t *testing.T) {
	resetForTest()
	Register("cpu.limit", fakeFactory("cpu.limit"))
	Register("memory.high", fakeFactory("memory.high"))
	_, err := Build("nope", Env{})
	if err == nil {
		t.Fatal("expected error for unknown action")
	}
	for _, n := range []string{"nope", "cpu.limit", "memory.high"} {
		if !strings.Contains(err.Error(), n) {
			t.Errorf("error %q does not mention %q", err, n)
		}
	}
	a, err := Build("cpu.limit", Env{})
	if err != nil || a.Describe().Action != "cpu.limit" {
		t.Fatalf("Build known: %v, %v", a, err)
	}
}

func TestNamesSorted(t *testing.T) {
	resetForTest()
	for _, n := range []string{"c", "a", "b"} {
		Register(n, fakeFactory(n))
	}
	got := strings.Join(Names(), ",")
	if got != "a,b,c" {
		t.Fatalf("Names = %s", got)
	}
}
