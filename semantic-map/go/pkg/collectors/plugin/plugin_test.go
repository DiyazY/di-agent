package plugin

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/DiyazY/di-agent/pkg/types"
)

// drive runs serve over the given request lines and returns its reply lines and
// result. serve reads until EOF, so the input ends where the agent would close stdin.
func drive(t *testing.T, setup func(Hello) (CollectFunc, error), lines ...string) ([]string, error) {
	t.Helper()
	got, _, err := driveErr(t, setup, lines...)
	return got, err
}

// driveErr is drive that also returns what serve wrote to stderr.
func driveErr(t *testing.T, setup func(Hello) (CollectFunc, error), lines ...string) ([]string, string, error) {
	t.Helper()
	var out, errOut strings.Builder
	err := serve(strings.NewReader(strings.Join(lines, "\n")+"\n"), &out, &errOut, setup)
	got := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if out.Len() == 0 {
		got = nil
	}
	return got, errOut.String(), err
}

const hello = `{"op":"hello","protocol":1,"name":"fx","node_id":"n1","interval_ms":10000,"config":{"device":0}}`

func TestServeHelloError(t *testing.T) {
	got, err := drive(t, func(Hello) (CollectFunc, error) {
		return nil, errors.New("no device")
	}, hello, `{"op":"collect","seq":1,"now_unix":5}`)
	if len(got) != 1 || got[0] != `{"error":"no device"}` {
		t.Fatalf("replies = %q, want one hello refusal", got)
	}
	if err == nil || !strings.Contains(err.Error(), "no device") {
		t.Fatalf("serve returned %v, want the setup error", err)
	}
}

func TestServeHelloPassesFields(t *testing.T) {
	var seen Hello
	got, err := drive(t, func(h Hello) (CollectFunc, error) {
		seen = h
		return func(time.Time) ([]*types.MetricSample, []types.MetricType, error) { return nil, nil, nil }, nil
	}, hello)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	if len(got) != 1 || got[0] != `{"protocol":1}` {
		t.Fatalf("replies = %q", got)
	}
	if seen.Name != "fx" || seen.NodeID != "n1" || seen.Interval != 10*time.Second || seen.Config["device"] != float64(0) {
		t.Fatalf("setup saw %+v", seen)
	}
}

func TestServeCollect(t *testing.T) {
	var gotNow time.Time
	collect := func(now time.Time) ([]*types.MetricSample, []types.MetricType, error) {
		gotNow = now
		return []*types.MetricSample{{MetricType: "fx", Value: 1, TimestampUnix: now.Unix(), Unit: "u"}},
			[]types.MetricType{"fx", "fy"}, nil
	}
	got, err := drive(t, func(Hello) (CollectFunc, error) { return collect, nil },
		hello, `{"op":"collect","seq":42,"now_unix":1761160000}`)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("replies = %q", got)
	}
	var r struct {
		Seq       uint64                `json:"seq"`
		Available []types.MetricType    `json:"available"`
		Samples   []*types.MetricSample `json:"samples"`
		Error     string                `json:"error"`
	}
	if err := json.Unmarshal([]byte(got[1]), &r); err != nil {
		t.Fatalf("reply %q: %v", got[1], err)
	}
	if r.Seq != 42 || len(r.Samples) != 1 || len(r.Available) != 2 || r.Error != "" {
		t.Fatalf("reply = %+v", r)
	}
	if gotNow.Unix() != 1761160000 {
		t.Fatalf("collect saw now %v", gotNow)
	}
}

// "No data right now" is an empty list on the wire, never null and never an error.
func TestServeNoDataIsEmptySamples(t *testing.T) {
	got, _ := drive(t, func(Hello) (CollectFunc, error) {
		return func(time.Time) ([]*types.MetricSample, []types.MetricType, error) { return nil, nil, nil }, nil
	}, hello, `{"op":"collect","seq":3,"now_unix":1}`)
	if len(got) != 2 || got[1] != `{"seq":3,"samples":[]}` {
		t.Fatalf("replies = %q", got)
	}
}

func TestServeCollectError(t *testing.T) {
	got, err := drive(t, func(Hello) (CollectFunc, error) {
		return func(time.Time) ([]*types.MetricSample, []types.MetricType, error) {
			return nil, nil, errors.New("device gone")
		}, nil
	}, hello, `{"op":"collect","seq":9,"now_unix":1}`, `{"op":"collect","seq":10,"now_unix":2}`)
	if err != nil {
		t.Fatalf("a collect error is a reply, not the end of serving: %v", err)
	}
	if len(got) != 3 || got[1] != `{"seq":9,"error":"device gone"}` {
		t.Fatalf("replies = %q", got)
	}
}

func TestServeCollectBeforeHello(t *testing.T) {
	got, _ := drive(t, func(Hello) (CollectFunc, error) { return nil, nil }, `{"op":"collect","seq":1,"now_unix":1}`)
	if len(got) != 1 || !strings.Contains(got[0], `"seq":1`) || !strings.Contains(got[0], `"error"`) {
		t.Fatalf("replies = %q", got)
	}
}

func TestServeReturnsOnEOF(t *testing.T) {
	err := serve(strings.NewReader(""), io.Discard, io.Discard, func(Hello) (CollectFunc, error) { return nil, nil })
	if err != nil {
		t.Fatalf("serve on EOF = %v, want nil", err)
	}
}

func TestServeRejectsMalformedLine(t *testing.T) {
	_, err := drive(t, func(Hello) (CollectFunc, error) { return nil, nil }, "not json")
	if err == nil {
		t.Fatalf("serve accepted a line that is not JSON")
	}
}

// A panic in collect is that tick's error reply, and the plugin answers the next.
func TestServeCollectPanicIsErrorReply(t *testing.T) {
	n := 0
	got, err := drive(t, func(Hello) (CollectFunc, error) {
		return func(now time.Time) ([]*types.MetricSample, []types.MetricType, error) {
			n++
			if n == 1 {
				panic("index out of range")
			}
			return []*types.MetricSample{{MetricType: "fx", Value: 1, TimestampUnix: now.Unix(), Unit: "u"}}, nil, nil
		}, nil
	}, hello, `{"op":"collect","seq":1,"now_unix":5}`, `{"op":"collect","seq":2,"now_unix":6}`)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	if len(got) != 3 || !strings.Contains(got[1], `"seq":1`) || !strings.Contains(got[1], "collect panicked: index out of range") {
		t.Fatalf("replies = %q", got)
	}
	if !strings.HasPrefix(got[2], `{"seq":2,"samples":[{`) {
		t.Fatalf("the tick after a panic = %q, want samples", got[2])
	}
}

// An op this version does not know gets an error reply echoing its seq, so the
// agent's reads stay in step.
func TestServeUnknownOp(t *testing.T) {
	got, err := drive(t, func(Hello) (CollectFunc, error) {
		return func(time.Time) ([]*types.MetricSample, []types.MetricType, error) { return nil, nil, nil }, nil
	}, hello, `{"op":"flush","seq":7}`)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	if len(got) != 2 || got[1] != `{"seq":7,"error":"unknown op \"flush\""}` {
		t.Fatalf("replies = %q", got)
	}
}

// A setup that returns no collect function and no error cannot serve a collect;
// the hello is refused rather than accepted and every tick failed.
func TestServeSetupNilCollectRefused(t *testing.T) {
	got, err := drive(t, func(Hello) (CollectFunc, error) { return nil, nil }, hello)
	if len(got) != 1 || got[0] != `{"error":"setup returned no collect function"}` {
		t.Fatalf("replies = %q", got)
	}
	if err == nil {
		t.Fatal("serve accepted a nil collect function")
	}
}

// NaN and ±Inf are not JSON. They are dropped before encoding, with one stderr
// line per type, so the reply carries the finite samples and Serve keeps going.
func TestServeDropsNonFiniteSamples(t *testing.T) {
	got, stderr, err := driveErr(t, func(Hello) (CollectFunc, error) {
		return func(now time.Time) ([]*types.MetricSample, []types.MetricType, error) {
			return []*types.MetricSample{
				{MetricType: "ok", Value: 1, Unit: "u"},
				{MetricType: "bad", Value: math.NaN(), Unit: "u"},
				{MetricType: "bad", Value: math.Inf(1), Unit: "u", Subject: "disk:sda"},
				{MetricType: "worse", Value: math.Inf(-1), Unit: "u"},
				nil,
			}, nil, nil
		}, nil
	}, hello, `{"op":"collect","seq":1,"now_unix":5}`, `{"op":"collect","seq":2,"now_unix":6}`)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("replies = %q", got)
	}
	var r struct {
		Samples []*types.MetricSample `json:"samples"`
	}
	if err := json.Unmarshal([]byte(got[1]), &r); err != nil {
		t.Fatalf("reply %q: %v", got[1], err)
	}
	if len(r.Samples) != 1 || r.Samples[0].MetricType != "ok" {
		t.Fatalf("samples = %+v, want only the finite one", r.Samples)
	}
	if n := strings.Count(stderr, "\n"); n != 4 {
		t.Fatalf("stderr = %q, want one line per type per tick", stderr)
	}
	if !strings.Contains(stderr, `dropped 2 sample(s) of "bad"`) || !strings.Contains(stderr, `dropped 1 sample(s) of "worse"`) {
		t.Fatalf("stderr = %q", stderr)
	}
}

// available nil is left out, so the agent infers it; available empty is sent as
// [], the plugin saying it can produce nothing now.
func TestServeAvailableAbsentVersusEmpty(t *testing.T) {
	for _, tc := range []struct {
		avail []types.MetricType
		want  string
	}{
		{nil, `{"seq":1,"samples":[]}`},
		{[]types.MetricType{}, `{"seq":1,"available":[],"samples":[]}`},
	} {
		got, _ := drive(t, func(Hello) (CollectFunc, error) {
			return func(time.Time) ([]*types.MetricSample, []types.MetricType, error) { return nil, tc.avail, nil }, nil
		}, hello, `{"op":"collect","seq":1,"now_unix":5}`)
		if len(got) != 2 || got[1] != tc.want {
			t.Errorf("available %#v: replies = %q, want %s", tc.avail, got, tc.want)
		}
	}
}

// A reply that cannot be encoded — a NaN in a range, which the value filter does
// not see — is that tick's error reply, and Serve answers the next request.
func TestServeUnencodableReplyIsErrorReply(t *testing.T) {
	n := 0
	got, err := drive(t, func(Hello) (CollectFunc, error) {
		return func(now time.Time) ([]*types.MetricSample, []types.MetricType, error) {
			n++
			s := &types.MetricSample{MetricType: "fx", Value: 1, TimestampUnix: now.Unix(), Unit: "u"}
			if n == 1 {
				s.Range = &[2]float64{0, math.NaN()}
			}
			return []*types.MetricSample{s}, nil, nil
		}, nil
	}, hello, `{"op":"collect","seq":1,"now_unix":5}`, `{"op":"collect","seq":2,"now_unix":6}`)
	if err != nil {
		t.Fatalf("serve returned %v; an unencodable reply must not end it", err)
	}
	if len(got) != 3 || !strings.HasPrefix(got[1], `{"seq":1,"error":"encode reply: `) {
		t.Fatalf("replies = %q", got)
	}
	if !strings.HasPrefix(got[2], `{"seq":2,"samples":[{`) {
		t.Fatalf("next tick = %q", got[2])
	}
}

func TestServeUnhealthyIsAReplyNotAnError(t *testing.T) {
	got, _ := drive(t, func(Hello) (CollectFunc, error) {
		return func(time.Time) ([]*types.MetricSample, []types.MetricType, error) {
			return nil, nil, Unhealthy("daemon refused the connection")
		}, nil
	}, hello, `{"op":"collect","seq":4,"now_unix":1}`)
	if len(got) != 2 || got[1] != `{"seq":4,"samples":[],"unhealthy":"daemon refused the connection"}` {
		t.Fatalf("replies = %q", got)
	}
}
