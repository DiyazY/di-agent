package statemap

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// JournalFileName is the append-only log beside persist.file. The previous
// generation is JournalFileName + ".1"; there is never more than one.
const JournalFileName = "journal.ndjson"

const (
	journalRotateAt   = int64(64 << 20)
	journalFlushEvery = time.Second
	journalErrorEvery = time.Minute
)

// LogStats is what GET /state/journal reports under "log".
type LogStats struct {
	File         string `json:"file"`
	Bytes        int64  `json:"bytes"`         // current generation's size
	Indexed      int    `json:"indexed"`       // decision ids resolvable from either generation
	CorruptLines int    `json:"corrupt_lines"` // digest mismatch, undecodable, or partial tail, both generations, since open
	ForeignLines int    `json:"foreign_lines"` // owner differs, both generations, since open
}

// logLine is one NDJSON line: the event's fields flattened, plus the writer's
// identity and a digest of everything before it.
type logLine struct {
	Owner string `json:"owner"`
	Inc   uint64 `json:"inc"`
	Event
	Digest string `json:"digest,omitempty"`
}

// logOffset locates a line: gen 0 is the current file, 1 is ".1".
type logOffset struct {
	gen uint8
	off int64
}

// logWriter is *os.File in production; tests wrap it to fail.
type logWriter interface {
	io.Writer
	Sync() error
	Close() error
}

// journalFile is the durable extension of the in-memory journal: every event as
// one digested line, decisions written and synced as they are appended, the rest
// on the flusher's tick. It never takes Journal.mu, and a failure to write is
// logged and counted, never returned: the in-memory journal and the snapshot still
// hold the events.
type journalFile struct {
	mu       sync.Mutex
	path     string
	owner    string
	inc      uint64
	maxInc   uint64 // highest incarnation among this owner's verified lines at open
	w        logWriter
	size     int64
	buf      []byte
	index    map[string]logOffset
	stats    LogStats
	rotateAt int64
	dirty    bool // the last write failed: the next write starts with "\n"
	lastErr  time.Time
	logf     func(string, ...any)
	now      func() time.Time
	closed   bool
	stop     chan struct{}
	done     chan struct{}
}

const digestKey = `,"digest":"`

// encodeLine marshals the line once without a digest, digests those bytes, and
// splices the digest in place of the closing brace.
func encodeLine(owner string, inc uint64, e Event) ([]byte, error) {
	b, err := json.Marshal(logLine{Owner: owner, Inc: inc, Event: e})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	out := make([]byte, 0, len(b)+len(digestKey)+64+3)
	out = append(out, b[:len(b)-1]...)
	out = append(out, digestKey...)
	out = append(out, hex.EncodeToString(sum[:])...)
	return append(out, '"', '}', '\n'), nil
}

// decodeLine verifies the digest encodeLine appended and decodes the line.
func decodeLine(raw []byte) (logLine, bool) {
	b := bytes.TrimSuffix(raw, []byte("\n"))
	i := bytes.LastIndex(b, []byte(digestKey))
	if i < 0 || len(b) != i+len(digestKey)+64+2 || !bytes.HasSuffix(b, []byte(`"}`)) {
		return logLine{}, false
	}
	body := make([]byte, 0, i+1)
	body = append(append(body, b[:i]...), '}')
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != string(b[i+len(digestKey):i+len(digestKey)+64]) {
		return logLine{}, false
	}
	var l logLine
	if err := json.Unmarshal(b, &l); err != nil {
		return logLine{}, false
	}
	return l, true
}

// openJournalFile indexes both generations and opens the current one for append.
// It never refuses a damaged file: bad lines are counted, and a partial tail on
// the current file is cut so a new line cannot glue onto it.
func openJournalFile(path, owner string, inc uint64, logf func(string, ...any)) (*journalFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f := &journalFile{
		path: path, owner: owner, inc: inc,
		index:    map[string]logOffset{},
		rotateAt: journalRotateAt,
		logf:     logf,
		now:      time.Now,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	if _, err := f.scan(1); err != nil {
		return nil, err
	}
	end, err := f.scan(0)
	if err != nil {
		return nil, err
	}
	if end >= 0 {
		if err := os.Truncate(path, end); err != nil {
			return nil, err
		}
	}
	if err := f.openAppend(); err != nil {
		return nil, err
	}
	go f.flusher()
	return f, nil
}

func (f *journalFile) genPath(gen uint8) string {
	if gen == 1 {
		return f.path + ".1"
	}
	return f.path
}

// scan indexes one generation. It returns the offset just past the last
// complete line when the file ends in a partial one, and -1 otherwise.
func (f *journalFile) scan(gen uint8) (int64, error) {
	h, err := os.Open(f.genPath(gen))
	if errors.Is(err, os.ErrNotExist) {
		return -1, nil
	}
	if err != nil {
		return -1, err
	}
	defer h.Close()
	r := bufio.NewReaderSize(h, 1<<20)
	var off int64
	for {
		raw, err := r.ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return -1, err
		}
		if errors.Is(err, io.EOF) {
			if len(raw) == 0 {
				return -1, nil
			}
			f.stats.CorruptLines++
			return off, nil
		}
		start := off
		off += int64(len(raw))
		if len(raw) == 1 { // the recovery newline after a failed write
			continue
		}
		l, ok := decodeLine(raw)
		switch {
		case !ok:
			f.stats.CorruptLines++
		case l.Owner != f.owner:
			f.stats.ForeignLines++
		default:
			f.maxInc = max(f.maxInc, l.Inc)
			if l.Kind == EventDecision && l.Target != "" {
				f.index[l.Target] = logOffset{gen: gen, off: start}
			}
		}
	}
}

func (f *journalFile) openAppend() error {
	h, err := os.OpenFile(f.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	st, err := h.Stat()
	if err != nil {
		h.Close()
		return err
	}
	f.w, f.size = h, st.Size()
	return nil
}

func (f *journalFile) flusher() {
	defer close(f.done)
	t := time.NewTicker(journalFlushEvery)
	defer t.Stop()
	for {
		select {
		case <-f.stop:
			return
		case <-t.C:
			f.mu.Lock()
			if len(f.buf) > 0 {
				f.flush(false)
			}
			f.mu.Unlock()
		}
	}
}

// Append adds an event to the log. A decision is written and synced before
// Append returns, and indexed only if that succeeded; anything else waits for
// the next flush.
func (f *journalFile) Append(e Event) {
	line, err := encodeLine(f.owner, f.inc, e)
	if err != nil { // this event only: the buffer and the file are untouched
		f.logf("journal log: encoding %s %s: %v", e.Kind, e.Target, err)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	rel := int64(len(f.buf))
	f.buf = append(f.buf, line...)
	if e.Kind != EventDecision {
		return
	}
	base, err := f.flush(true)
	if err == nil && e.Target != "" {
		f.index[e.Target] = logOffset{gen: 0, off: base + rel}
	}
}

// flush is the single write path; f.mu is held. It returns the offset in the
// current file at which the buffer landed.
func (f *journalFile) flush(sync bool) (int64, error) {
	if len(f.buf) == 0 && !sync {
		return f.size, nil
	}
	if f.w == nil {
		if err := f.openAppend(); err != nil {
			return 0, f.fail(err)
		}
	}
	if f.size > 0 && f.size+int64(len(f.buf)) > f.rotateAt {
		if err := f.rotate(); err != nil {
			return 0, f.fail(err)
		}
	}
	if f.dirty {
		n, err := f.w.Write([]byte("\n"))
		f.size += int64(n)
		if err != nil {
			return 0, f.fail(err)
		}
		f.dirty = false
	}
	base := f.size
	if len(f.buf) > 0 {
		n, err := f.w.Write(f.buf)
		f.size += int64(n)
		if err != nil {
			return 0, f.fail(err)
		}
	}
	if sync {
		if err := f.w.Sync(); err != nil {
			return 0, f.fail(err)
		}
	}
	f.buf = f.buf[:0]
	return base, nil
}

// rotate moves the current file to ".1", replacing the previous generation, and
// starts a fresh one.
func (f *journalFile) rotate() error {
	f.w.Close()
	f.w = nil
	if err := os.Rename(f.path, f.path+".1"); err != nil {
		return err
	}
	for id, o := range f.index {
		if o.gen == 1 {
			delete(f.index, id)
		} else {
			f.index[id] = logOffset{gen: 1, off: o.off}
		}
	}
	f.dirty = false // the fragment, if any, went with the old file
	return f.openAppend()
}

// fail drops the buffer, marks the file dirty, and logs at most once per
// journalErrorEvery. f.mu is held.
func (f *journalFile) fail(err error) error {
	f.dirty = true
	f.buf = f.buf[:0]
	if now := f.now(); now.Sub(f.lastErr) >= journalErrorEvery {
		f.logf("journal log: %v (further errors muted for %s)", err, journalErrorEvery)
		f.lastErr = now
	}
	return err
}

// Decision reads one decision back from the log by id.
func (f *journalFile) Decision(id string) (*Decision, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.index[id]
	if !ok {
		return nil, false, nil
	}
	p := f.genPath(o.gen)
	h, err := os.Open(p)
	if err != nil {
		return nil, false, err
	}
	defer h.Close()
	if _, err := h.Seek(o.off, io.SeekStart); err != nil {
		return nil, false, err
	}
	raw, err := bufio.NewReader(h).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	l, ok := decodeLine(raw)
	if !ok || l.Target != id || l.Decision == nil {
		delete(f.index, id)
		return nil, false, fmt.Errorf("journal log: line at %s:%d for %s does not verify", p, o.off, id)
	}
	return l.Decision, true, nil
}

// Stats reports the log's size, index and the lines skipped since open.
func (f *journalFile) Stats() LogStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.stats
	s.File, s.Bytes, s.Indexed = f.path, f.size, len(f.index)
	return s
}

// Close stops the flusher, flushes and syncs what is buffered, and closes the
// file. It is idempotent.
func (f *journalFile) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	f.mu.Unlock()
	close(f.stop)
	<-f.done
	f.mu.Lock()
	defer f.mu.Unlock()
	_, err := f.flush(true) // reopens the file when a failed rotation left none
	if f.w != nil {
		if cerr := f.w.Close(); err == nil {
			err = cerr
		}
		f.w = nil
	}
	return err
}
