// Package gossip provides immutable, content-addressed objects kept separately
// from the agent's local semantic map, with bounded pull-based dissemination
// over explicitly registered peers. It does not perform discovery.
package gossip

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
)

const (
	SchemaVersion  = 1
	MaxObjectBytes = 64 * 1024
	MaxStoreBytes  = 4 * 1024 * 1024
	MaxObjects     = 128
)

var (
	ErrInvalidObject = errors.New("invalid gossip object")
	ErrInvalidHash   = errors.New("hash must be 64 lowercase hexadecimal characters")
	ErrHashMismatch  = errors.New("gossip object hash mismatch")
	ErrTooLarge      = errors.New("gossip object exceeds 64 KiB")
	ErrFull          = errors.New("gossip object store is full")
	ErrPersistence   = errors.New("gossip persistence failed")
)

// Object is the versioned wire envelope. OriginID is a claim, not an
// authenticated identity. Payload is opaque JSON, never applied to local state.
type Object struct {
	SchemaVersion int             `json:"schema_version"`
	OriginID      string          `json:"origin_id"`
	PayloadType   string          `json:"payload_type"`
	Payload       json.RawMessage `json:"payload"`
}

func (o Object) validate() error {
	if o.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: unsupported schema_version %d", ErrInvalidObject, o.SchemaVersion)
	}
	if strings.TrimSpace(o.OriginID) == "" || strings.TrimSpace(o.PayloadType) == "" {
		return fmt.Errorf("%w: origin_id and payload_type are required", ErrInvalidObject)
	}
	if !json.Valid(o.Payload) || bytes.Equal(bytes.TrimSpace(o.Payload), []byte("null")) {
		return fmt.Errorf("%w: payload must be non-null JSON", ErrInvalidObject)
	}
	return nil
}

// Encode constructs the local wire bytes. Receivers preserve these bytes rather
// than re-encoding JSON: this is byte identity, not semantic JSON equivalence.
func Encode(originID, payloadType string, payload json.RawMessage) ([]byte, error) {
	o := Object{SchemaVersion, originID, payloadType, payload}
	if err := o.validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(o)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidObject, err)
	}
	if len(data) > MaxObjectBytes {
		return nil, ErrTooLarge
	}
	return data, nil
}

func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func ValidateHash(hash string) error {
	if len(hash) != sha256.Size*2 {
		return ErrInvalidHash
	}
	for _, c := range hash {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return ErrInvalidHash
		}
	}
	return nil
}

func Verify(hash string, data []byte) error {
	if err := ValidateHash(hash); err != nil {
		return err
	}
	if len(data) > MaxObjectBytes {
		return ErrTooLarge
	}
	if Hash(data) != hash {
		return ErrHashMismatch
	}
	var o Object
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&o); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidObject, err)
	}
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return fmt.Errorf("%w: expected one JSON object", ErrInvalidObject)
	}
	return o.validate()
}

// Memory retains at most MaxObjects objects and MaxStoreBytes wire bytes.
// When configured by NewPersistent, it also durably stores admitted objects.
// Full stores reject new content rather than silently evicting existing content.
// Stored and returned bytes are copied; duplicate delivery consumes no capacity.
type Memory struct {
	mu      sync.RWMutex
	objects map[string][]byte
	bytes   int
	dir     string
}

func NewMemory() *Memory {
	return &Memory{objects: make(map[string][]byte)}
}

// Put verifies content before admission. The result is true only for a new object.
func (m *Memory) Put(hash string, data []byte) (bool, error) {
	if len(data) > MaxObjectBytes {
		return false, ErrTooLarge
	}
	owned := bytes.Clone(data)
	if err := Verify(hash, owned); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.objects[hash]; ok {
		return false, nil
	}
	if len(m.objects) >= MaxObjects || m.bytes+len(owned) > MaxStoreBytes {
		return false, ErrFull
	}
	if m.dir != "" {
		if err := persistObject(m.dir, hash, owned); err != nil {
			return false, fmt.Errorf("%w: %w", ErrPersistence, err)
		}
	}
	m.objects[hash] = owned
	m.bytes += len(owned)
	return true, nil
}

func (m *Memory) Get(hash string) ([]byte, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	data, ok := m.objects[hash]
	return bytes.Clone(data), ok
}

func (m *Memory) Has(hash string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.objects[hash]
	return ok
}

type Inventory struct {
	Hashes []string `json:"hashes"`
	Bytes  int      `json:"bytes"`
}

// Inventory is bounded by MaxObjects and sorted for stable advertisements.
func (m *Memory) Inventory() Inventory {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := Inventory{Hashes: make([]string, 0, len(m.objects)), Bytes: m.bytes}
	for hash := range m.objects {
		out.Hashes = append(out.Hashes, hash)
	}
	slices.Sort(out.Hashes)
	return out
}
