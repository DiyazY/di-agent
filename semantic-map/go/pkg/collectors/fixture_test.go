package collectors

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/types"
)

// TestMain doubles the test binary as an exec plugin: with DI_FIXTURE set it runs
// the fixture in that mode and exits, so the exec tests drive a real child process
// without building a separate program.
func TestMain(m *testing.M) {
	if mode := os.Getenv("DI_FIXTURE"); mode != "" {
		fixtureMain(mode)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fixtureMain speaks wire protocol v1 on stdin/stdout, misbehaving as mode says.
// linger keeps running after stdin closes; linger-ignore-term also ignores SIGTERM;
// slow-hello takes 2s to answer each hello.
// It records each hello it receives in $DI_FIXTURE_DIR/hellos so a test can count
// restarts from the plugin's side.
func fixtureMain(mode string) {
	dir := os.Getenv("DI_FIXTURE_DIR")
	if mode == "linger-ignore-term" {
		signal.Ignore(syscall.SIGTERM)
	}
	in := bufio.NewReaderSize(os.Stdin, 1<<20)
	out := bufio.NewWriter(os.Stdout)
	reply := func(v any) {
		b, _ := json.Marshal(v)
		out.Write(append(b, '\n'))
		out.Flush()
	}
	for {
		line, err := in.ReadBytes('\n')
		if err != nil {
			if strings.HasPrefix(mode, "linger") {
				// Does not take stdin closing as the signal to stop.
				time.Sleep(time.Hour)
			}
			return
		}
		var req struct {
			Op      string `json:"op"`
			Seq     uint64 `json:"seq"`
			NowUnix int64  `json:"now_unix"`
		}
		if json.Unmarshal(line, &req) != nil {
			return
		}
		switch req.Op {
		case "hello":
			if dir != "" {
				f, err := os.OpenFile(filepath.Join(dir, "hellos"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
				if err == nil {
					f.WriteString("hello\n")
					f.Close()
				}
			}
			switch mode {
			case "slow-hello":
				// A cold start: an interpreter loading its imports, a device opening.
				time.Sleep(2 * time.Second)
				reply(HelloReply{Protocol: ProtocolVersion})
			case "hello-error":
				reply(HelloReply{Error: "fixture refuses: no device"})
				return
			case "bad-protocol":
				reply(HelloReply{Protocol: 2})
			case "doctor":
				reply(HelloReply{Protocol: ProtocolVersion, Doctor: []string{"/bin/sh", "-c", "echo checked $DI_COLLECTOR_NAME; exit 3"}})
			default:
				reply(HelloReply{Protocol: ProtocolVersion})
			}
		case "collect":
			switch mode {
			case "garbage":
				out.WriteString("not json\n")
				out.Flush()
				continue
			case "hang":
				continue
			case "hang-grandchild":
				// A wrapper's own child is usually what hangs; it must die with the
				// plugin, not be orphaned.
				gc := exec.Command("sleep", "60")
				if gc.Start() == nil {
					os.WriteFile(filepath.Join(dir, "grandchild"), []byte(strconv.Itoa(gc.Process.Pid)), 0o600)
				}
				continue
			case "exit-after-collect":
				reply(CollectReply{Seq: req.Seq, Samples: fixtureSamples(req.NowUnix)})
				return
			case "stale-seq":
				reply(CollectReply{Seq: req.Seq - 1, Samples: fixtureSamples(req.NowUnix)})
				continue
			case "crash-once":
				marker := filepath.Join(dir, "crashed")
				if _, err := os.Stat(marker); err != nil {
					os.WriteFile(marker, nil, 0o600)
					os.Exit(3)
				}
			case "noisy-stderr":
				for i := 0; i < 100; i++ {
					fmt.Fprintf(os.Stderr, "noise line %d\n", i)
				}
			case "unhealthy":
				reply(CollectReply{Seq: req.Seq, Unhealthy: "fixture source refused"})
				continue
			case "huge":
				out.WriteString(`{"seq":` + fmt.Sprint(req.Seq) + `,"error":"` + strings.Repeat("x", 5<<20) + "\"}\n")
				out.Flush()
				continue
			}
			reply(CollectReply{Seq: req.Seq, Samples: fixtureSamples(req.NowUnix)})
		}
	}
}

// fixtureSamples is the good reply: one unranged and one ranged node-level sample.
// The first carries the names of the environment variables the child was given, so
// a test can check the environment is minimal.
func fixtureSamples(now int64) []*types.MetricSample {
	var keys []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return []*types.MetricSample{
		{MetricType: "fx_load", Value: 0.5, TimestampUnix: now, Unit: "load",
			Labels: map[string]string{"env": strings.Join(keys, ",")}},
		{MetricType: "fx_share", Value: 0.25, TimestampUnix: now, Unit: "share", Range: &[2]float64{0, 1}},
	}
}
