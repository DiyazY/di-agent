package collectors

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/DiyazY/di-agent/pkg/contracts"
	"github.com/DiyazY/di-agent/pkg/spec"
	"github.com/DiyazY/di-agent/pkg/types"
)

const (
	// maxExecLine bounds one reply line. A plugin that writes more is faulted rather
	// than allowed to grow the agent's memory without limit.
	maxExecLine = 4 << 20

	// stderrBurst lines per stderrWindow are copied to the log; the rest are counted.
	stderrBurst  = 20
	stderrWindow = 10 * time.Second
	// stderrMaxLine truncates one relayed stderr line.
	stderrMaxLine = 4 << 10

	// shutdownGrace is waited after closing stdin, and again after SIGTERM, before
	// the next, harder step of Close.
	shutdownGrace = 2 * time.Second
	// reapWait bounds the wait for a process to be reaped after SIGKILL. One that
	// is still not gone (stuck in uninterruptible sleep) is logged and abandoned
	// rather than allowed to hang the caller.
	reapWait = 2 * time.Second
	// exitLogEvery spaces the log lines about a plugin that exits between ticks;
	// the exits in between are counted into the next line.
	exitLogEvery = time.Minute
	// relayDrain bounds how long a reaped process's stderr may still be relayed
	// before the pipe is closed under it, in case a grandchild holds it open.
	relayDrain = 200 * time.Millisecond
	// startTimeout bounds launching the plugin and its hello, at Start and at every
	// restart, when it is longer than the collect timeout. A plugin's cold start (an
	// interpreter loading its imports from an SD card on a Raspberry Pi, a device
	// opening) can take seconds, and a restart is a cold start too: held to the
	// tick's deadline, a plugin that needs longer would fail every restart after
	// its first fault and back off for good. A restart that outlives the tick is
	// abandoned by the supervisor, which skips the member while it runs, so the
	// other members' ticks do not wait for it.
	startTimeout = 30 * time.Second
)

// ExecCollector runs an out-of-process plugin speaking wire protocol v1 over its
// stdin and stdout, and presents it as a CollectorContract.
//
// The process is started once and kept. Any fault — a timeout, a crash, a line
// that is not JSON, a reply to another seq, an error reply, an oversized line —
// kills it and returns an error; the next Collect starts it again and re-sends
// hello first. Spacing those restarts is the supervisor's backoff, not this type's.
type ExecCollector struct {
	env     Env
	name    string
	sp      spec.ExecSpec
	timeout time.Duration
	config  map[string]any
	logf    func(string, ...any)

	// extraEnv is appended to the child's environment; tests use it to select a
	// fixture mode.
	extraEnv []string
	// grace is shutdownGrace; tests shorten it.
	grace time.Duration

	// stop is closed by Close before it takes mu, so a Start or Collect waiting on
	// a reply gives up at once instead of holding Close for up to startTimeout.
	stop     chan struct{}
	stopOnce sync.Once

	// mu serialises Start, Collect and Close, which own proc.
	mu      sync.Mutex
	proc    *execProc
	started bool
	closed  bool
	seq     uint64
	clock   nowClock

	// availMu guards the last successful reply's available list and drops.
	availMu   sync.Mutex
	available []types.MetricType
	drops     []contracts.SampleDrop
	health    error
	doctor    []string // the hello's, kept across restarts until the next hello

	restarts atomic.Int64

	// The stderr rate limit is per member, not per process, so a plugin that
	// crashes in a loop cannot reset its allowance by restarting.
	logMu       sync.Mutex
	windowStart time.Time
	windowLines int
	suppressed  int

	// Exits between ticks are logged at most once per exitLogEvery. Guarded by mu.
	lastExitLog   time.Time
	exitsUnlogged int
}

// execProc is one running instance of the plugin.
type execProc struct {
	cmd    *exec.Cmd
	stdin  *os.File
	stdout *os.File
	stderr *os.File
	lines  chan lineResult
	quit   chan struct{} // closed when the agent abandons the process
	done   chan struct{} // closed once Wait has returned
	relay  chan struct{} // closed when the stderr relay has ended
}

type lineResult struct {
	line []byte
	err  error
}

// NewExecCollector prepares the plugin entry name; nothing runs until Start. A
// non-positive timeout takes the default, min(interval/2, 5s).
func NewExecCollector(env Env, name string, sp spec.ExecSpec, timeout time.Duration, config map[string]any) *ExecCollector {
	timeout = resolveTimeout(env, timeout)
	if config == nil {
		config = map[string]any{}
	}
	logf := env.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &ExecCollector{env: env, name: name, sp: sp, timeout: timeout, config: config, logf: logf,
		grace: shutdownGrace, stop: make(chan struct{})}
}

// Start spawns the plugin and completes hello within startTimeout, or the collect
// timeout when that is longer. An error means the plugin could not be run, did
// not answer or refused the hello; the daemon refuses to start on it.
func (e *ExecCollector) Start() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return errors.New("collector closed")
	}
	if e.proc != nil {
		return nil
	}
	e.started = true
	return e.spawn(e.startDeadline(time.Now()))
}

// startDeadline is when a launch begun at now must have answered hello:
// startTimeout, or the collect timeout when that is longer.
func (e *ExecCollector) startDeadline(now time.Time) time.Time {
	return now.Add(max(e.timeout, startTimeout))
}

// Collect sends one collect request and returns the stamped samples. A plugin
// that faulted earlier, or exited since the last tick, is restarted first.
//
// A restart is a cold start, so its hello has the startup allowance (Start), not
// the tick's deadline. The collect shares the tick's deadline with the restart: a
// restart that used it up returns an error without collecting, and the plugin,
// now running, collects from the next tick. Such a restart outlives the
// supervisor's wait, which abandons the call and skips the member until it ends;
// the other members' ticks do not wait for it.
func (e *ExecCollector) Collect() ([]*types.MetricSample, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, errors.New("collector closed")
	}
	e.availMu.Lock()
	e.drops = nil
	e.availMu.Unlock()
	begin := time.Now()
	deadline := begin.Add(e.timeout)
	if e.proc != nil {
		// A plugin that exited between ticks is not a fault of this tick; reap it
		// and start a fresh one instead of writing into a dead pipe. It is still a
		// plugin that does not keep running, so it is logged.
		select {
		case <-e.proc.done:
			e.logExit(e.proc.cmd.ProcessState)
			e.fault(nil)
		default:
		}
	}
	if e.proc == nil {
		if e.started {
			e.restarts.Add(1)
		}
		e.started = true
		if err := e.spawn(e.startDeadline(begin)); err != nil {
			return nil, fmt.Errorf("restart: %w", err)
		}
		if took := time.Since(begin); took >= e.timeout {
			return nil, fmt.Errorf("restart: plugin took %s to start and answer hello, past this tick's %s timeout; it collects from the next tick",
				took.Round(time.Millisecond), e.timeout)
		}
	}
	e.seq++
	seq := e.seq
	now := e.clock.next(time.Now())
	line, err := e.roundTrip(CollectRequest{Op: "collect", Seq: seq, NowUnix: now}, deadline)
	if err != nil {
		return nil, e.fault(err)
	}
	r, err := decodeReply(line)
	if err != nil {
		return nil, e.fault(err)
	}
	if err := checkCollect(r, seq); err != nil {
		return nil, e.fault(err)
	}
	samples, avail, drops := stampReply(e.name, e.env.NodeID, now, r)
	e.availMu.Lock()
	e.available = avail
	e.drops = drops
	e.health = replyHealth(r)
	e.availMu.Unlock()
	return samples, nil
}

// Health is the last reply's unhealthy reason, or nil.
func (e *ExecCollector) Health() error {
	e.availMu.Lock()
	defer e.availMu.Unlock()
	return e.health
}

// DoctorCommand is the doctor the plugin declared in its last hello, or nil.
func (e *ExecCollector) DoctorCommand() []string {
	e.availMu.Lock()
	defer e.availMu.Unlock()
	return append([]string(nil), e.doctor...)
}

// RunDoctor runs the declared doctor as the plugin runs: in the plugin's directory
// (when its command is a path) with the plugin's minimal environment, in its own
// process group, killed with it when ctx ends.
func (e *ExecCollector) RunDoctor(ctx context.Context) ([]byte, error) {
	argv := e.DoctorCommand()
	if len(argv) == 0 {
		return nil, errors.New("no doctor declared")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if filepath.Base(e.sp.Command) != e.sp.Command {
		if path, err := exec.LookPath(e.sp.Command); err == nil {
			if abs, err := filepath.Abs(path); err == nil {
				cmd.Dir = filepath.Dir(abs)
			}
		}
	}
	cmd.Env = e.childEnv()
	ownGroup(cmd)
	cmd.Cancel = func() error { signalGroup(cmd, syscall.SIGKILL); return nil }
	cmd.WaitDelay = time.Second
	return cmd.CombinedOutput()
}

// TakeDrops returns what the last Collect dropped from the plugin's reply —
// malformed and duplicate samples, invalid available entries — and forgets it.
func (e *ExecCollector) TakeDrops() []contracts.SampleDrop {
	e.availMu.Lock()
	defer e.availMu.Unlock()
	d := e.drops
	e.drops = nil
	return d
}

// SourceID is the entry name, so it is stable across restarts.
func (e *ExecCollector) SourceID() string { return e.name }

// AvailableMetrics is the last successful reply's available list, or the types
// that reply carried.
func (e *ExecCollector) AvailableMetrics() []types.MetricType {
	e.availMu.Lock()
	defer e.availMu.Unlock()
	return append([]types.MetricType(nil), e.available...)
}

// Restarts counts how many times a faulted plugin has been started again. It is
// an atomic so status can read it while a Collect is holding the process lock.
func (e *ExecCollector) Restarts() int64 { return e.restarts.Load() }

// Close shuts the plugin down: stdin is closed so a well-behaved plugin exits on
// EOF; then SIGTERM, then SIGKILL, each after the shutdown grace. The error is nil
// when the plugin exited on EOF, and says which signal it took otherwise, so a
// caller joining Close errors learns which plugins ignore the protocol's stop.
func (e *ExecCollector) Close() error {
	e.stopOnce.Do(func() { close(e.stop) })
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	p := e.proc
	e.proc = nil
	if p == nil {
		return nil
	}
	close(p.quit)
	p.stdin.Close()
	var err error
	select {
	case <-p.done:
	case <-time.After(e.grace):
		signalGroup(p.cmd, syscall.SIGTERM)
		select {
		case <-p.done:
			err = fmt.Errorf("plugin did not exit within %s of stdin closing; stopped by SIGTERM", e.grace)
		case <-time.After(e.grace):
			signalGroup(p.cmd, syscall.SIGKILL)
			err = fmt.Errorf("plugin ignored stdin closing and SIGTERM for %s each; killed", e.grace)
			if !e.awaitReap(p) {
				err = fmt.Errorf("%w, and not reaped within %s; abandoned", err, reapWait)
			}
		}
	}
	// Whatever the plugin started and left running goes with it.
	signalGroup(p.cmd, syscall.SIGKILL)
	p.release()
	return err
}

// awaitReap waits up to reapWait for a killed process to be reaped, and logs and
// gives up on one that is not.
func (e *ExecCollector) awaitReap(p *execProc) bool {
	select {
	case <-p.done:
		return true
	case <-time.After(reapWait):
		pid := 0
		if p.cmd.Process != nil {
			pid = p.cmd.Process.Pid
		}
		e.logf("collector[%s]: plugin pid %d not reaped %s after SIGKILL; abandoning it", e.name, pid, reapWait)
		return false
	}
}

// logExit logs a plugin that exited between ticks, with its exit status, at most
// once per exitLogEvery; the exits in between are counted into the next line.
// Called with mu held.
func (e *ExecCollector) logExit(ps *os.ProcessState) {
	e.exitsUnlogged++
	now := time.Now()
	if !e.lastExitLog.IsZero() && now.Sub(e.lastExitLog) < exitLogEvery {
		return
	}
	status := "unknown status"
	if ps != nil {
		status = ps.String()
	}
	if e.exitsUnlogged > 1 {
		e.logf("collector[%s]: WARNING plugin exited between ticks (%s; %d exits since the last notice); restarting it. A plugin must keep running and exit only when stdin closes",
			e.name, status, e.exitsUnlogged)
	} else {
		e.logf("collector[%s]: WARNING plugin exited between ticks (%s); restarting it. A plugin must keep running and exit only when stdin closes", e.name, status)
	}
	e.lastExitLog, e.exitsUnlogged = now, 0
}

// spawn starts the process and sends hello, answered before deadline; on any
// failure the process is reaped and e.proc stays nil. Called with mu held.
func (e *ExecCollector) spawn(deadline time.Time) error {
	p, err := e.startProcess()
	if err != nil {
		return err
	}
	e.proc = p
	hello := newHello(e.env, e.name, e.config)
	line, err := e.roundTrip(hello, deadline)
	if err != nil {
		return e.fault(fmt.Errorf("hello: %w", err))
	}
	var r HelloReply
	if err := json.Unmarshal(line, &r); err != nil {
		return e.fault(fmt.Errorf("hello reply is not JSON: %w", err))
	}
	if err := checkHello(r); err != nil {
		return e.fault(err)
	}
	e.availMu.Lock()
	e.doctor = r.Doctor
	e.availMu.Unlock()
	return nil
}

// startProcess runs the command with a minimal environment in its own directory.
// The pipes are made here rather than by os/exec so the agent owns their ends:
// Wait then never closes a pipe under a reader, and stdin can take a deadline.
func (e *ExecCollector) startProcess() (*execProc, error) {
	path, err := exec.LookPath(e.sp.Command)
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", e.sp.Command, err)
	}
	if path, err = filepath.Abs(path); err != nil {
		return nil, fmt.Errorf("start %s: %w", e.sp.Command, err)
	}
	cmd := exec.Command(path, e.sp.Args...)
	// A command given as a path runs in its own directory, so a plugin can read
	// files shipped beside it by relative path. A bare name found on PATH runs in
	// the agent's working directory: its directory is a shared bin directory, not
	// the plugin's.
	if filepath.Base(e.sp.Command) != e.sp.Command {
		cmd.Dir = filepath.Dir(path)
	}
	cmd.Env = e.childEnv()
	ownGroup(cmd)

	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, errW
	err = cmd.Start()
	// The child holds its own copies; the agent keeps only its ends, so EOF on
	// stdout and stderr means the child (and anything it spawned) let go.
	inR.Close()
	outW.Close()
	errW.Close()
	if err != nil {
		inW.Close()
		outR.Close()
		errR.Close()
		return nil, fmt.Errorf("start %s: %w", e.sp.Command, err)
	}
	p := &execProc{cmd: cmd, stdin: inW, stdout: outR, stderr: errR,
		lines: make(chan lineResult), quit: make(chan struct{}),
		done: make(chan struct{}), relay: make(chan struct{})}
	go func() {
		cmd.Wait()
		close(p.done)
	}()
	go readLines(outR, p.lines, p.quit)
	go func() {
		e.relayStderr(errR)
		close(p.relay)
	}()
	return p, nil
}

// childEnv is the minimal environment: PATH, HOME and LANG from the agent if set,
// the node and entry name, and nothing else the agent happens to carry.
func (e *ExecCollector) childEnv() []string {
	var env []string
	for _, k := range []string{"PATH", "HOME", "LANG"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	env = append(env, "DI_NODE_ID="+e.env.NodeID, "DI_COLLECTOR_NAME="+e.name)
	return append(env, e.extraEnv...)
}

// roundTrip writes one request line and reads one reply line, both before
// deadline. Called with mu held and e.proc set.
func (e *ExecCollector) roundTrip(req any, deadline time.Time) ([]byte, error) {
	p := e.proc
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	// A pipe from os.Pipe accepts a deadline on the platforms the agent runs on;
	// where it does not, the request is a single small write and the read below
	// still bounds the call.
	p.stdin.SetWriteDeadline(deadline)
	if _, err := p.stdin.Write(append(b, '\n')); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}
	// Never negative: a deadline already passed is reported as 0s, not "-3ms".
	wait := max(time.Until(deadline), 0)
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case r := <-p.lines:
		if r.err != nil {
			return nil, r.err
		}
		return r.line, nil
	case <-t.C:
		return nil, fmt.Errorf("no reply within %s", wait.Round(time.Millisecond))
	case <-e.stop:
		return nil, errors.New("collector closed")
	}
}

// fault kills the current process group, reaps the process (waiting at most
// reapWait), and returns err, noting the exit status when the process had already
// ended on its own. A nil err only reaps. Called with mu held.
func (e *ExecCollector) fault(err error) error {
	p := e.proc
	e.proc = nil
	if p == nil {
		return err
	}
	exitedAlready := false
	select {
	case <-p.done:
		exitedAlready = true
	default:
	}
	close(p.quit)
	signalGroup(p.cmd, syscall.SIGKILL)
	p.stdin.Close()
	e.awaitReap(p)
	p.release()
	if err == nil {
		return nil
	}
	if exitedAlready && p.cmd.ProcessState != nil {
		return fmt.Errorf("%w (plugin %s)", err, p.cmd.ProcessState)
	}
	return err
}

// release closes the agent's pipe ends of a reaped process. stderr gets a short
// drain first so a crashing plugin's last words reach the log; closing then ends
// both reader goroutines even if a grandchild still holds the other ends.
func (p *execProc) release() {
	select {
	case <-p.relay:
	case <-time.After(relayDrain):
	}
	p.stdout.Close()
	p.stderr.Close()
}

// readLines delivers stdout one line at a time until EOF, an error, or a line
// longer than maxExecLine, each of which ends it. It stops sending once the agent
// has abandoned the process.
func readLines(r io.Reader, out chan<- lineResult, quit <-chan struct{}) {
	br := bufio.NewReaderSize(r, 64<<10)
	send := func(lr lineResult) bool {
		select {
		case out <- lr:
			return true
		case <-quit:
			return false
		}
	}
	for {
		var line []byte
		for {
			chunk, err := br.ReadSlice('\n')
			if len(line)+len(chunk) > maxExecLine+1 {
				send(lineResult{err: fmt.Errorf("reply line exceeds %d MiB", maxExecLine>>20)})
				return
			}
			line = append(line, chunk...)
			if err == bufio.ErrBufferFull {
				continue
			}
			if err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) {
					err = errors.New("plugin closed stdout")
				}
				send(lineResult{err: err})
				return
			}
			break
		}
		if !send(lineResult{line: line}) {
			return
		}
	}
}

// relayStderr copies the plugin's stderr to the log, prefixed with the entry
// name, at most stderrBurst lines per stderrWindow. The first line dropped in a
// window is announced once; the count of dropped lines opens the next window and
// counts against its allowance.
func (e *ExecCollector) relayStderr(r io.Reader) {
	br := bufio.NewReaderSize(r, stderrMaxLine)
	for {
		chunk, err := br.ReadSlice('\n')
		if len(chunk) > 0 {
			line := strings.TrimRight(string(chunk), "\r\n")
			// Swallow the rest of an overlong line so it is relayed once, truncated.
			for err == bufio.ErrBufferFull {
				_, err = br.ReadSlice('\n')
			}
			e.logStderr(line)
		}
		if err != nil {
			return
		}
	}
}

func (e *ExecCollector) logStderr(line string) {
	e.logMu.Lock()
	defer e.logMu.Unlock()
	now := time.Now()
	if now.Sub(e.windowStart) >= stderrWindow {
		e.windowStart, e.windowLines = now, 0
		if e.suppressed > 0 {
			e.logf("collector[%s]: %d stderr lines suppressed", e.name, e.suppressed)
			e.suppressed = 0
			e.windowLines++
		}
	}
	if e.windowLines < stderrBurst {
		e.windowLines++
		e.logf("collector[%s]: %s", e.name, line)
		return
	}
	e.suppressed++
	if e.suppressed == 1 {
		e.logf("collector[%s]: stderr exceeds %d lines per %s; suppressing the rest of this window", e.name, stderrBurst, stderrWindow)
	}
}
