package autonomy

import (
	"strings"
	"testing"

	"github.com/DiyazY/di-agent/pkg/statemap"
)

func TestTheEvidenceNamesTheMachine(t *testing.T) {
	e := Evidence{Facts: []statemap.Fact{
		{Key: "machine_model", Value: "Raspberry Pi 4 Model B Rev 1.4"},
		statemap.NumberFact("cpu_cores", 4, "cores"), {Key: "cpu_model", Value: "Arm Cortex-A72"},
		statemap.NumberFact("memory_total", 8<<30, "bytes"), {Key: "os", Value: "Ubuntu 22.04.2 LTS"},
	}}
	want := "Machine: Raspberry Pi 4 Model B Rev 1.4, 4 CPU cores (Arm Cortex-A72), 8.0 GiB memory, Ubuntu 22.04.2 LTS\n"
	if got := stateText(e, ""); !strings.Contains(got, want) {
		t.Errorf("evidence:\n%s\nwant %q", got, want)
	}
	if got := stateText(Evidence{}, ""); strings.Contains(got, "Machine:") {
		t.Error("a machine line with no facts")
	}
}
