//go:build unix

package collectors

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestExecTimeoutKillsProcessGroup(t *testing.T) {
	e, dir := fixture(t, "hang-grandchild", 300*time.Millisecond, nil)
	if err := e.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := e.Collect(); err == nil {
		t.Fatal("Collect of a hanging plugin succeeded")
	}
	b, err := os.ReadFile(filepath.Join(dir, "grandchild"))
	if err != nil {
		t.Fatalf("fixture did not record its grandchild: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	// The orphaned grandchild is reaped by init, so it may linger as a zombie
	// briefly; it is gone once signal 0 reports no such process.
	deadline := time.Now().Add(3 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("grandchild %d survived the fault (kill 0: %v)", pid, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestExecUnhealthyReplyAndDeclaredDoctor(t *testing.T) {
	e, _ := fixture(t, "unhealthy", 2*time.Second, nil)
	if err := e.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := e.Collect(); err != nil {
		t.Fatalf("Collect of an unhealthy reply = %v, want no fault", err)
	}
	if err := e.Health(); err == nil || err.Error() != "fixture source refused" {
		t.Fatalf("Health = %v, want the plugin's reason", err)
	}
	if argv := e.DoctorCommand(); argv != nil {
		t.Fatalf("DoctorCommand = %v for a plugin that declared none", argv)
	}

	d, _ := fixture(t, "doctor", 2*time.Second, nil)
	if err := d.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := d.Collect(); err != nil || d.Health() != nil {
		t.Fatalf("Collect = %v, Health = %v; want a healthy tick", err, d.Health())
	}
	if argv := d.DoctorCommand(); len(argv) != 3 || argv[0] != "/bin/sh" {
		t.Fatalf("DoctorCommand = %v, want the hello's", argv)
	}
	out, err := d.RunDoctor(context.Background())
	if strings.TrimSpace(string(out)) != "checked fx" || err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("RunDoctor = %q, %v; want its output and exit status", out, err)
	}
}
