// Package plugin runs an out-of-process collector written in Go: it speaks wire
// protocol v1 on stdin and stdout so the plugin's author writes only what to
// measure.
//
// It imports the standard library and pkg/types and nothing of the agent, so a
// plugin binary stays small and does not move when the agent's internals do. The
// message shapes are therefore copies of pkg/collectors' wire types; tests keep
// them in step with the agent's.
package plugin

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"time"

	"github.com/DiyazY/di-agent/pkg/types"
)

// ProtocolVersion is the wire protocol this package speaks.
const ProtocolVersion = 1

// maxLine bounds one request line, the same 4 MiB the agent allows a reply.
const maxLine = 4 << 20

// Hello is what a plugin is told when the agent starts or reconnects it: its
// entry name, the node it runs on, how often it will be asked, and its entry's
// config block (an empty map when the entry has none).
type Hello struct {
	Name     string
	NodeID   string
	Interval time.Duration
	Config   map[string]any
}

// CollectFunc answers one collect request. now is the agent's clock for the
// request, which is the timestamp a sample should carry.
//
// No samples is "no data right now", not an error. available, when non-nil, is
// every type the plugin can produce now, and an empty non-nil slice says it can
// produce none; when nil the agent takes the types in samples. A sample whose
// value is NaN or ±Inf cannot be sent as JSON: it is left out of the reply and a
// line on stderr says so. An error made by Unhealthy says the plugin's source did
// not answer: the reply carries the reason, and the agent asks again next tick. Any
// other error is a fault: the agent restarts the plugin with backoff.
type CollectFunc func(now time.Time) (samples []*types.MetricSample, available []types.MetricType, err error)

// Serve answers the agent on stdin and stdout until stdin closes, which is how the
// agent stops a plugin. setup is called on every hello; an error from it, or a nil
// CollectFunc, is sent as the hello's refusal and Serve returns it, since the
// agent will not use a plugin that refused. A line that is not a request ends
// Serve with an error. Notes about dropped samples go to stderr.
func Serve(setup func(Hello) (CollectFunc, error)) error {
	return serve(os.Stdin, os.Stdout, os.Stderr, setup)
}

// request is any message from the agent; the fields an op does not use stay zero.
type request struct {
	Op         string         `json:"op"`
	Protocol   int            `json:"protocol"`
	Name       string         `json:"name"`
	NodeID     string         `json:"node_id"`
	IntervalMS int64          `json:"interval_ms"`
	Config     map[string]any `json:"config"`
	Seq        uint64         `json:"seq"`
	NowUnix    int64          `json:"now_unix"`
}

func (r request) hello() Hello {
	cfg := r.Config
	if cfg == nil {
		cfg = map[string]any{}
	}
	return Hello{Name: r.Name, NodeID: r.NodeID, Interval: time.Duration(r.IntervalMS) * time.Millisecond, Config: cfg}
}

type helloReply struct {
	Protocol int    `json:"protocol,omitempty"`
	Error    string `json:"error,omitempty"`
}

// collectReply always carries samples, so "no data" is an empty list on the wire.
// Available is a pointer so nil is left out (the agent infers it) while an empty
// list is sent as [].
type collectReply struct {
	Seq       uint64                `json:"seq"`
	Available *[]types.MetricType   `json:"available,omitempty"`
	Samples   []*types.MetricSample `json:"samples"`
	Unhealthy string                `json:"unhealthy,omitempty"`
}

// unhealthy is the error Unhealthy makes.
type unhealthy string

func (u unhealthy) Error() string { return string(u) }

// Unhealthy is the error a CollectFunc returns when its source did not answer —
// the daemon it reads is down, the device is gone — with the reason. It is not a
// fault: the agent marks the plugin unhealthy and keeps asking every tick.
func Unhealthy(reason string) error { return unhealthy(reason) }

type errorReply struct {
	Seq   uint64 `json:"seq"`
	Error string `json:"error"`
}

func serve(in io.Reader, out, errOut io.Writer, setup func(Hello) (CollectFunc, error)) error {
	br := bufio.NewReaderSize(in, 64<<10)
	bw := bufio.NewWriter(out)
	// Each reply is flushed at once: the agent waits for it before anything else.
	writeLine := func(b []byte) error {
		if _, err := bw.Write(append(b, '\n')); err != nil {
			return err
		}
		return bw.Flush()
	}
	reply := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		return writeLine(b)
	}
	var collect CollectFunc
	for {
		line, err := readLine(br)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			return fmt.Errorf("request is not JSON: %w", err)
		}
		switch req.Op {
		case "hello":
			c, err := callSetup(setup, req.hello())
			if err == nil && c == nil {
				err = errors.New("setup returned no collect function")
			}
			if err != nil {
				if rerr := reply(helloReply{Error: err.Error()}); rerr != nil {
					return rerr
				}
				return fmt.Errorf("setup: %w", err)
			}
			collect = c
			if err := reply(helloReply{Protocol: ProtocolVersion}); err != nil {
				return err
			}
		case "collect":
			// A reply that cannot be encoded (a NaN in a range, say) costs that
			// tick, as a collect error would, not the process.
			b, err := json.Marshal(answer(collect, req, errOut))
			if err != nil {
				b, err = json.Marshal(errorReply{Seq: req.Seq, Error: fmt.Sprintf("encode reply: %v", err)})
				if err != nil {
					return err
				}
			}
			if err := writeLine(b); err != nil {
				return err
			}
		default:
			// One reply per request keeps the agent's reads in step even for an
			// operation this version does not know.
			if err := reply(errorReply{Seq: req.Seq, Error: fmt.Sprintf("unknown op %q", req.Op)}); err != nil {
				return err
			}
		}
	}
}

// callSetup runs setup, turning a panic into a refusal like an error.
func callSetup(setup func(Hello) (CollectFunc, error), h Hello) (c CollectFunc, err error) {
	defer func() {
		if p := recover(); p != nil {
			c, err = nil, fmt.Errorf("setup panicked: %v", p)
		}
	}()
	return setup(h)
}

// answer runs one collect, turning an error or a panic into an error reply so a
// bad tick costs that tick and not the process. Non-finite samples are dropped
// here, before encoding, and each type that lost one gets a line on errOut; a nil
// sample is dropped silently.
func answer(collect CollectFunc, req request, errOut io.Writer) (r any) {
	if collect == nil {
		return errorReply{Seq: req.Seq, Error: "collect before hello"}
	}
	defer func() {
		if p := recover(); p != nil {
			r = errorReply{Seq: req.Seq, Error: fmt.Sprintf("collect panicked: %v", p)}
		}
	}()
	samples, available, err := collect(time.Unix(req.NowUnix, 0))
	var u unhealthy
	if errors.As(err, &u) {
		return collectReply{Seq: req.Seq, Samples: []*types.MetricSample{}, Unhealthy: string(u)}
	}
	if err != nil {
		return errorReply{Seq: req.Seq, Error: err.Error()}
	}
	kept := make([]*types.MetricSample, 0, len(samples))
	var dropped []types.MetricType
	count := map[types.MetricType]int{}
	for _, s := range samples {
		if s == nil {
			continue
		}
		if math.IsNaN(s.Value) || math.IsInf(s.Value, 0) {
			if count[s.MetricType] == 0 {
				dropped = append(dropped, s.MetricType)
			}
			count[s.MetricType]++
			continue
		}
		kept = append(kept, s)
	}
	for _, mt := range dropped {
		fmt.Fprintf(errOut, "plugin: dropped %d sample(s) of %q with a NaN or infinite value\n", count[mt], mt)
	}
	cr := collectReply{Seq: req.Seq, Samples: kept}
	if available != nil {
		cr.Available = &available
	}
	return cr
}

// readLine returns one line without its newline. A final line without a newline
// still counts; a line over maxLine is an error.
func readLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(line)+len(chunk) > maxLine+1 {
			return nil, fmt.Errorf("request line exceeds %d MiB", maxLine>>20)
		}
		line = append(line, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && len(line) > 0 {
			return line, nil
		}
		if err != nil {
			return nil, err
		}
		return line[:len(line)-1], nil
	}
}
