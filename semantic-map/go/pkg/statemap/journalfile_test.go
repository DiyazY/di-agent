package statemap

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTestLog(t *testing.T, dir string, owner string) *journalFile {
	t.Helper()
	f, err := openJournalFile(filepath.Join(dir, JournalFileName), owner, 1, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func decisionEvent(id string, seq uint64) Event {
	d := &Decision{ID: id, Kind: "estimate", Question: "q", Answer: map[string]any{"v": 1.0}}
	return Event{Seq: seq, Kind: EventDecision, Target: id, Actor: "agent", Decision: d}
}

func observationEvent(seq uint64) Event {
	return Event{Seq: seq, Kind: EventPropertyAdmitted, Target: "p"}
}

// fileLines splits a log file into its newline-terminated lines (a tail without
// a newline is returned as the last element).
func fileLines(t *testing.T, path string) [][]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			out = append(out, b)
			break
		}
		out = append(out, b[:i+1])
		b = b[i+1:]
	}
	return out
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

func mustResolve(t *testing.T, f *journalFile, id string) {
	t.Helper()
	d, ok, err := f.Decision(id)
	if err != nil || !ok || d == nil || d.ID != id {
		t.Fatalf("Decision(%s) = %v, %v, %v", id, d, ok, err)
	}
}

func TestDecisionLinesAreWrittenAtOnceAndOthersOnFlush(t *testing.T) {
	dir := t.TempDir()
	f := openTestLog(t, dir, "n1")
	path := filepath.Join(dir, JournalFileName)

	f.Append(observationEvent(1))
	f.Append(decisionEvent("d-1.1", 2))
	if n := len(fileLines(t, path)); n != 2 {
		t.Fatalf("after a decision: %d lines, want 2", n)
	}
	f.Append(observationEvent(3))
	if n := len(fileLines(t, path)); n != 2 {
		t.Fatalf("an observation alone was written: %d lines, want 2", n)
	}
	f.mu.Lock()
	f.flush(false)
	f.mu.Unlock()
	if n := len(fileLines(t, path)); n != 3 {
		t.Fatalf("after flush: %d lines, want 3", n)
	}
	if got, want := f.Stats().Bytes, fileSize(t, path); got != want {
		t.Fatalf("Stats().Bytes = %d, file is %d", got, want)
	}
}

func TestEveryLineCarriesOwnerIncSeqAndAVerifiableDigest(t *testing.T) {
	dir := t.TempDir()
	f := openTestLog(t, dir, "n1")
	f.Append(observationEvent(7))
	f.Append(decisionEvent("d-1.1", 8))
	lines := fileLines(t, filepath.Join(dir, JournalFileName))
	for i, want := range []uint64{7, 8} {
		l, ok := decodeLine(lines[i])
		if !ok {
			t.Fatalf("line %d does not decode: %s", i, lines[i])
		}
		if l.Owner != "n1" || l.Inc != 1 || l.Seq != want {
			t.Fatalf("line %d: owner %q inc %d seq %d", i, l.Owner, l.Inc, l.Seq)
		}
	}
	bad := bytes.Clone(lines[1])
	i := bytes.Index(bad, []byte(`"question":"q"`))
	if i < 0 {
		t.Fatalf("no question field in %s", bad)
	}
	bad[i+len(`"question":"`)] = 'x'
	if _, ok := decodeLine(bad); ok {
		t.Fatal("a line with a changed question still verifies")
	}
}

func TestRotationKeepsOnePreviousGeneration(t *testing.T) {
	dir := t.TempDir()
	f := openTestLog(t, dir, "n1")
	f.mu.Lock()
	f.rotateAt = 1024
	f.mu.Unlock()
	path := filepath.Join(dir, JournalFileName)
	prev := path + ".1"

	var ids []string
	var firstPrev int64 = -1
	replaced := false
	for n := 1; n < 1000 && !replaced; n++ {
		id := fmt.Sprintf("d-1.%d", n)
		ids = append(ids, id)
		f.Append(decisionEvent(id, uint64(n)))
		st, err := os.Stat(prev)
		if err != nil {
			continue
		}
		if firstPrev < 0 {
			firstPrev = st.Size()
			// the first .1 must be followed by enough decisions to rotate again
			continue
		}
		// a second rotation replaces .1 with what was the current file; its
		// content (the first id it holds) changes even if its size does not.
		first, _ := decodeLine(fileLines(t, prev)[0])
		if first.Target != ids[0] {
			replaced = true
		}
	}
	if !replaced {
		t.Fatal("journal.ndjson.1 was never replaced")
	}

	count := 0
	for _, p := range []string{prev, path} {
		for _, raw := range fileLines(t, p) {
			l, ok := decodeLine(raw)
			if !ok {
				t.Fatalf("bad line in %s: %s", p, raw)
			}
			if l.Kind == EventDecision {
				count++
				mustResolve(t, f, l.Target)
			}
		}
	}
	if got := f.Stats().Indexed; got != count {
		t.Fatalf("Indexed = %d, decision lines in both files = %d", got, count)
	}
	d, ok, err := f.Decision(ids[0])
	if d != nil || ok || err != nil {
		t.Fatalf("an id older than both generations: %v, %v, %v", d, ok, err)
	}
}

func TestCorruptAndForeignLinesAreSkippedAndCounted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, JournalFileName)
	f, err := openJournalFile(path, "n1", 1, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	f.Append(decisionEvent("d-1.1", 1))
	f.Append(decisionEvent("d-1.2", 2))
	f.Close()

	wrong, _ := encodeLine("n1", 1, decisionEvent("d-1.3", 3))
	j := bytes.LastIndex(wrong, []byte(`"digest":"`)) + len(`"digest":"`)
	if wrong[j] == '0' {
		wrong[j] = '1'
	} else {
		wrong[j] = '0'
	}
	foreign, _ := encodeLine("n2", 1, decisionEvent("d-2.1", 1))
	h, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	h.Write(wrong)
	h.Write(foreign)
	h.Write([]byte(`{"kind":"decision"`))
	h.Close()

	g := openTestLog(t, dir, "n1")
	st := g.Stats()
	if st.Indexed != 2 || st.CorruptLines != 2 || st.ForeignLines != 1 {
		t.Fatalf("stats = %+v, want indexed 2, corrupt 2, foreign 1", st)
	}
	mustResolve(t, g, "d-1.1")
	mustResolve(t, g, "d-1.2")
}

func TestOpenTruncatesAPartialLastLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, JournalFileName)
	f, err := openJournalFile(path, "n1", 1, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	f.Append(decisionEvent("d-1.1", 1))
	f.Append(decisionEvent("d-1.2", 2))
	f.Close()
	h, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	h.Write([]byte(`{"kind":"decision"`))
	h.Close()

	g, err := openJournalFile(path, "n1", 1, t.Logf)
	if err != nil {
		t.Fatalf("open with a partial tail refused: %v", err)
	}
	if c := g.Stats().CorruptLines; c != 1 {
		t.Fatalf("CorruptLines = %d, want 1", c)
	}
	g.Append(decisionEvent("d-1.3", 3))
	g.Close()

	h2 := openTestLog(t, dir, "n1")
	st := h2.Stats()
	if st.Indexed != 3 || st.CorruptLines != 0 {
		t.Fatalf("third open: %+v, want indexed 3, corrupt 0", st)
	}
	mustResolve(t, h2, "d-1.3")
}

func TestOpenWithOnlyThePreviousGeneration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, JournalFileName)
	f, err := openJournalFile(path, "n1", 1, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	f.Append(decisionEvent("d-1.1", 1))
	f.Append(decisionEvent("d-1.2", 2))
	f.Close()
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}

	g := openTestLog(t, dir, "n1")
	if n := g.Stats().Indexed; n != 2 {
		t.Fatalf("Indexed = %d, want 2", n)
	}
	for _, id := range []string{"d-1.1", "d-1.2"} {
		mustResolve(t, g, id)
		g.mu.Lock()
		gen := g.index[id].gen
		g.mu.Unlock()
		if gen != 1 {
			t.Fatalf("%s is in gen %d, want 1", id, gen)
		}
	}
	g.Append(decisionEvent("d-1.3", 3))
	mustResolve(t, g, "d-1.3")
	if n := len(fileLines(t, path)); n != 1 {
		t.Fatalf("fresh gen 0 holds %d lines, want 1", n)
	}
}

func TestRestartReindexes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, JournalFileName)
	f, err := openJournalFile(path, "n1", 1, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 5; n++ {
		f.Append(decisionEvent(fmt.Sprintf("d-1.%d", n), uint64(n)))
	}
	f.Close()

	g := openTestLog(t, dir, "n1")
	for n := 1; n <= 5; n++ {
		mustResolve(t, g, fmt.Sprintf("d-1.%d", n))
	}
	if got, want := g.Stats().Bytes, fileSize(t, path); got != want {
		t.Fatalf("Stats().Bytes = %d, file is %d", got, want)
	}
}

// halfWriter fails its first Write after writing half the bytes; failing makes
// every Write fail without writing anything.
type halfWriter struct {
	logWriter
	failed bool
}

func (h *halfWriter) Write(p []byte) (int, error) {
	if h.failed {
		return h.logWriter.Write(p)
	}
	h.failed = true
	n, _ := h.logWriter.Write(p[:len(p)/2])
	return n, errors.New("disk full")
}

type failingWriter struct{ logWriter }

func (failingWriter) Write(p []byte) (int, error) { return 0, errors.New("disk full") }

func TestAWriteErrorDoesNotGlueTheNextLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, JournalFileName)
	f, err := openJournalFile(path, "n1", 1, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.w = &halfWriter{logWriter: f.w}
	f.mu.Unlock()
	f.Append(decisionEvent("d-1.1", 1))
	f.Append(decisionEvent("d-1.2", 2))
	mustResolve(t, f, "d-1.2")
	f.Close()

	g := openTestLog(t, dir, "n1")
	st := g.Stats()
	if st.CorruptLines != 1 || st.Indexed != 1 {
		t.Fatalf("stats = %+v, want corrupt 1, indexed 1", st)
	}
	mustResolve(t, g, "d-1.2")
	lines := fileLines(t, path)
	if len(lines) != 2 || lines[1][0] != '{' {
		t.Fatalf("lines = %q", lines)
	}
}

func TestWriteErrorsAreLoggedOncePerMinute(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	var mu sync.Mutex
	logged := 0
	logf := func(string, ...any) { mu.Lock(); logged++; mu.Unlock() }
	f, err := openJournalFile(filepath.Join(dir, JournalFileName), "n1", 1, logf)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.mu.Lock()
	f.w = failingWriter{f.w}
	f.now = c.now
	f.mu.Unlock()

	for n := 1; n <= 10; n++ {
		f.Append(decisionEvent(fmt.Sprintf("d-1.%d", n), uint64(n)))
		f.mu.Lock()
		c.advance(time.Second)
		f.mu.Unlock()
	}
	mu.Lock()
	if logged != 1 {
		t.Fatalf("logged %d times in 10 s, want 1", logged)
	}
	mu.Unlock()
	f.mu.Lock()
	c.advance(61 * time.Second)
	f.mu.Unlock()
	f.Append(decisionEvent("d-1.11", 11))
	mu.Lock()
	defer mu.Unlock()
	if logged != 2 {
		t.Fatalf("logged %d times after a minute, want 2", logged)
	}
}

func TestConcurrentAppendsAreRaceClean(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, JournalFileName)
	f, err := openJournalFile(path, "n1", 1, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for n := 0; n < 50; n++ {
				seq := uint64(g*50 + n)
				if n%2 == 0 {
					f.Append(decisionEvent(fmt.Sprintf("d-%d.%d", g, n), seq))
				} else {
					f.Append(observationEvent(seq))
				}
				_ = f.Stats()
			}
		}(g)
	}
	wg.Wait()
	f.Close()

	r := openTestLog(t, dir, "n1")
	for g := 0; g < 8; g++ {
		for n := 0; n < 50; n += 2 {
			mustResolve(t, r, fmt.Sprintf("d-%d.%d", g, n))
		}
	}
	if st := r.Stats(); st.CorruptLines != 0 || st.Indexed != 200 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestADecisionEvictedFromMemoryIsResolvedFromTheLog(t *testing.T) {
	m := New(Config{Owner: "n1"}, NewJournal(3))
	if err := m.OpenLog(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i := 0; i < 5; i++ {
		id := m.NewDecisionID("est")
		if m.Decide(id, "estimate", "q").Commit(map[string]any{"i": i}) == nil {
			t.Fatalf("commit %s refused", id)
		}
		ids = append(ids, id)
	}
	if _, ok := m.Journal().Decision(ids[0]); ok {
		t.Fatalf("%s should be evicted from a journal of 3", ids[0])
	}
	d, ok, err := m.Journal().DecisionFromLog(ids[0])
	if err != nil || !ok {
		t.Fatalf("DecisionFromLog(%s) = %v, %v", ids[0], ok, err)
	}
	if d.ID != ids[0] || d.Kind != "estimate" {
		t.Fatalf("log returned %+v", d)
	}
	if err := m.CloseLog(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenLogTwiceIsRefusedAndCloseIsIdempotent(t *testing.T) {
	m := New(Config{Owner: "n1"}, NewJournal(0))
	if _, ok := m.Journal().LogStats(); ok {
		t.Fatal("LogStats reports a log before OpenLog")
	}
	if d, ok, err := m.Journal().DecisionFromLog("x"); d != nil || ok || err != nil {
		t.Fatalf("DecisionFromLog without a log = %v, %v, %v", d, ok, err)
	}
	dir := t.TempDir()
	if err := m.OpenLog(dir); err != nil {
		t.Fatal(err)
	}
	if err := m.OpenLog(dir); err == nil {
		t.Fatal("a second OpenLog was accepted")
	}
	if s, ok := m.Journal().LogStats(); !ok || s.File != filepath.Join(dir, JournalFileName) {
		t.Fatalf("LogStats = %+v, %v", s, ok)
	}
	if err := m.CloseLog(); err != nil {
		t.Fatal(err)
	}
	if err := m.CloseLog(); err != nil {
		t.Fatalf("second CloseLog: %v", err)
	}
	if _, ok := m.Journal().LogStats(); ok {
		t.Fatal("LogStats reports a log after CloseLog")
	}
	// Appending after close does not panic or write.
	m.Decide(m.NewDecisionID("est"), "estimate", "q").Commit(nil)
}

// A crash after a save leaves a log whose tail carries seqs above what the
// snapshot restores. Those lines are history, not a conflict: indexed and
// resolvable, and the next life appends after them under its own ids.
func TestLogFromAnEarlierIncarnationWithHigherSeqIsIndexed(t *testing.T) {
	dir := t.TempDir()
	snap := filepath.Join(dir, "state.json")
	a := New(Config{Owner: "n1"}, NewJournal(0))
	if err := a.OpenLog(dir); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i := 1; i <= 20; i++ {
		id := a.NewDecisionID("est")
		a.Decide(id, "estimate", "q").Commit(nil)
		ids = append(ids, id)
		if i == 10 {
			if err := a.Save(snap); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := a.CloseLog(); err != nil { // no save: the simulated crash
		t.Fatal(err)
	}

	b := New(Config{Owner: "n1"}, NewJournal(0))
	r, err := b.Load(snap)
	if err != nil || !r.Loaded {
		t.Fatalf("Load = %+v, %v", r, err)
	}
	if b.Incarnation() != 2 {
		t.Fatalf("incarnation = %d, want 2", b.Incarnation())
	}
	if hw := b.Journal().Seq(); hw >= a.Journal().Seq() {
		t.Fatalf("restored high-water %d is not below the crashed life's %d", hw, a.Journal().Seq())
	}
	if err := b.OpenLog(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.CloseLog() })
	s, _ := b.Journal().LogStats()
	if s.Indexed != 20 || s.CorruptLines != 0 || s.ForeignLines != 0 {
		t.Fatalf("stats = %+v, want 20 indexed, nothing skipped", s)
	}
	if _, held := b.Journal().Decision(ids[19]); held {
		t.Fatalf("%s should not be in the restored journal", ids[19])
	}
	d, ok, err := b.Journal().DecisionFromLog(ids[19])
	if err != nil || !ok || d.ID != ids[19] {
		t.Fatalf("DecisionFromLog(%s) = %+v, %v, %v", ids[19], d, ok, err)
	}
	id := b.NewDecisionID("est")
	if !strings.HasPrefix(id, "est-2.") {
		t.Fatalf("new id %q, want prefix est-2.", id)
	}
	b.Decide(id, "estimate", "q").Commit(nil)
	s, _ = b.Journal().LogStats()
	if s.Indexed != 21 || s.CorruptLines != 0 {
		t.Fatalf("after a new decision stats = %+v", s)
	}
	if d, ok, err := b.Journal().DecisionFromLog(id); err != nil || !ok || d.ID != id {
		t.Fatalf("DecisionFromLog(%s) = %+v, %v, %v", id, d, ok, err)
	}
	lines := fileLines(t, filepath.Join(dir, JournalFileName))
	if l, ok := decodeLine(lines[len(lines)-1]); !ok || l.Target != id || l.Inc != 2 {
		t.Fatalf("last line = %+v, %v; want %s from incarnation 2", l, ok, id)
	}
}

// A cold start (no snapshot) over a surviving log must not reissue the earlier
// life's ids: OpenLog takes the incarnation after the highest the log holds.
func TestColdStartOverAnExistingLogTakesTheNextIncarnation(t *testing.T) {
	dir := t.TempDir()
	a := New(Config{Owner: "n1"}, NewJournal(0))
	if err := a.OpenLog(dir); err != nil {
		t.Fatal(err)
	}
	old := a.NewDecisionID("est")
	if a.Decide(old, "estimate", "life 1").Commit(nil) == nil {
		t.Fatalf("commit %s refused", old)
	}
	if err := a.CloseLog(); err != nil { // no Save: state.json never written
		t.Fatal(err)
	}

	b := New(Config{Owner: "n1"}, NewJournal(0))
	if err := b.OpenLog(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.CloseLog() })
	if b.Incarnation() != 2 {
		t.Fatalf("incarnation = %d, want 2", b.Incarnation())
	}
	id := b.NewDecisionID("est")
	if !strings.HasPrefix(id, "est-2.") || id == old {
		t.Fatalf("new id %q, want prefix est-2. and not %s", id, old)
	}
	if d, ok, _ := b.Journal().DecisionFromLog(id); ok {
		t.Fatalf("uncommitted %s resolves from the log as %+v", id, d)
	}
	if d, ok, err := b.Journal().DecisionFromLog(old); err != nil || !ok || d.Question != "life 1" {
		t.Fatalf("DecisionFromLog(%s) = %+v, %v, %v", old, d, ok, err)
	}
	b.Decide(id, "estimate", "life 2").Commit(nil)
	lines := fileLines(t, filepath.Join(dir, JournalFileName))
	if l, ok := decodeLine(lines[len(lines)-1]); !ok || l.Target != id || l.Inc != 2 {
		t.Fatalf("last line = %+v, %v; want %s from incarnation 2", l, ok, id)
	}
}

// A failed rotation leaves no open file; Close still flushes what is buffered.
func TestCloseFlushesAfterAFailedRotationLeftNoFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, JournalFileName)
	f, err := openJournalFile(path, "n1", 1, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	f.Append(observationEvent(1))
	f.mu.Lock()
	f.w.Close()
	f.w = nil
	f.mu.Unlock()
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	lines := fileLines(t, path)
	if len(lines) != 1 {
		t.Fatalf("lines = %q, want the buffered event", lines)
	}
	if l, ok := decodeLine(lines[0]); !ok || l.Seq != 1 {
		t.Fatalf("line = %+v, %v", l, ok)
	}
}
