package collectors

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/DiyazY/di-agent/internal/minimal"
	"github.com/DiyazY/di-agent/pkg/contracts"
	"github.com/DiyazY/di-agent/pkg/spec"
	"go.yaml.in/yaml/v3"
)

// Env is what every collector is built with: the node it observes, the agent's
// collection interval, and where it logs. Logf may be nil, which discards. Requests is
// the pods' memory requests the cgroup collector counts memory contention against; nil
// drops that condition.
type Env struct {
	NodeID   string
	Interval time.Duration
	Logf     func(format string, args ...any)
	Requests *minimal.Requests
}

// Factory builds the collector of one registered type. It decodes its own config
// strictly: a key it does not know is an error, so a misspelt option cannot be
// silently ignored.
type Factory func(env Env, config *yaml.Node) (contracts.CollectorContract, error)

var (
	registryMu sync.RWMutex
	factories  = map[string]Factory{}
)

// Register makes typ available as `type:` in sources.collectors. It is meant for
// init(); an empty or already registered type is a programming error and panics.
func Register(typ string, f Factory) {
	if typ == "" {
		panic("collectors: Register with an empty type")
	}
	if f == nil {
		panic("collectors: Register " + typ + " with a nil factory")
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := factories[typ]; dup {
		panic("collectors: type " + typ + " registered twice")
	}
	factories[typ] = f
}

// DefaultTimeout is the per-request timeout of an entry that sets none:
// min(interval/2, 5s), and 5s when the interval is unknown.
func DefaultTimeout(interval time.Duration) time.Duration {
	return resolveTimeout(Env{Interval: interval}, 0)
}

// buildHook, when set by a test, sees every ExecCollector Build makes before it is
// started, so a test can point it at the fixture.
var buildHook func(*ExecCollector)

// startable is implemented by the out-of-process adapters, which must complete
// hello before the daemon runs.
type startable interface{ Start() error }

// Build turns the configured entries into a running Supervisor, or nil when there
// are none (the collection loop is then disabled). Any failure refuses the whole
// list, naming the entry, and closes every member already built.
func Build(env Env, entries []spec.CollectorEntry) (*Supervisor, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	members := make([]Member, 0, len(entries))
	closeAll := func() {
		for _, m := range members {
			if c, ok := m.C.(io.Closer); ok {
				c.Close() //nolint:errcheck // already failing; the build error is the one reported
			}
		}
	}
	for i, e := range entries {
		m, err := buildMember(env, e)
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("sources.collectors[%d] %s: %w", i, e.Name, err)
		}
		members = append(members, m)
	}
	return NewSupervisor(env, members), nil
}

func buildMember(env Env, e spec.CollectorEntry) (m Member, err error) {
	// Set here so status and the adapters agree on it; NewSupervisor applies the
	// same default to a Member built by hand.
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout(env.Interval)
	}
	m = Member{Name: e.Name, Kind: e.Kind(), Timeout: timeout}
	switch m.Kind {
	case "type":
		registryMu.RLock()
		f, ok := factories[e.Type]
		registryMu.RUnlock()
		if !ok {
			return m, fmt.Errorf("unknown collector type %q", e.Type)
		}
		m.Type = e.Type
		m.C, err = f(env, &e.Config)
		return m, err
	case "exec", "http":
		cfg, err := configJSON(&e.Config)
		if err != nil {
			return m, err
		}
		if m.Kind == "exec" {
			ec := NewExecCollector(env, e.Name, *e.Exec, timeout, cfg)
			if buildHook != nil {
				buildHook(ec)
			}
			m.C = ec
		} else {
			m.C = NewHTTPCollector(env, e.Name, *e.HTTP, timeout, cfg)
		}
		if err := m.C.(startable).Start(); err != nil {
			// A failed Start may leave a half-started child behind.
			m.C.(io.Closer).Close() //nolint:errcheck
			return m, err
		}
		return m, nil
	}
	return m, errors.New("must name exactly one of type, exec or http")
}

// decodeStrict decodes n into out, refusing keys out does not declare. A zero
// node (no config given) leaves out at the defaults it was passed in with.
func decodeStrict(n *yaml.Node, out any) error {
	if n == nil || n.Kind == 0 {
		return nil
	}
	raw, err := yaml.Marshal(n)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
