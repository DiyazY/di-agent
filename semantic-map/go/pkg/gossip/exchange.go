package gossip

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	MaxInventoryBytes  = 16 * 1024
	MaxPeersPerRound   = 2
	MaxFetchesPerRound = 8
	RoundTimeout       = 5 * time.Second
)

var ErrInvalidInventory = errors.New("invalid gossip inventory")

// Exchange reconciles inventories with a bounded rotating subset of peers.
// It owns no membership or semantic state; the caller supplies current URLs.
type Exchange struct {
	mu      sync.Mutex
	store   *Memory
	client  *Client
	next    int
	offsets map[string]int
}

func NewExchange(store *Memory, client *Client) *Exchange {
	return &Exchange{store: store, client: client, offsets: make(map[string]int)}
}

type RoundResult struct {
	Peers     int
	Attempted int
	Added     int
}

// Round is sequential, including concurrent callers. Failed fetches consume the
// attempt budget too, so corrupt advertisements cannot make a round unbounded.
func (e *Exchange) Round(ctx context.Context, peerURLs []string) (RoundResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, RoundTimeout)
	defer cancel()

	present := make(map[string]bool, len(peerURLs))
	for _, url := range peerURLs {
		present[url] = true
	}
	for url := range e.offsets {
		if !present[url] {
			delete(e.offsets, url)
		}
	}
	var result RoundResult
	if len(peerURLs) == 0 {
		e.next = 0
		return result, nil
	}
	start := e.next % len(peerURLs)
	count := min(MaxPeersPerRound, len(peerURLs))
	// Advancing by one lets every peer lead a round, even when the first peer
	// consumes the deadline and the second is never reached.
	e.next = (start + 1) % len(peerURLs)
	var errs []error
	for i := range count {
		if err := ctx.Err(); err != nil {
			return result, errors.Join(append(errs, err)...)
		}
		url := peerURLs[(start+i)%len(peerURLs)]
		result.Peers++
		inventory, err := e.client.Inventory(ctx, url)
		if err != nil {
			errs = append(errs, fmt.Errorf("peer %s: %w", url, err))
			continue
		}
		if len(inventory.Hashes) == 0 {
			continue
		}
		offset := e.offsets[url] % len(inventory.Hashes)
		for j := range len(inventory.Hashes) {
			if result.Attempted >= MaxFetchesPerRound {
				break
			}
			if err := ctx.Err(); err != nil {
				return result, errors.Join(append(errs, err)...)
			}
			index := (offset + j) % len(inventory.Hashes)
			hash := inventory.Hashes[index]
			e.offsets[url] = (index + 1) % len(inventory.Hashes)
			if e.store.Has(hash) {
				continue
			}
			result.Attempted++
			data, err := e.client.Fetch(ctx, url, hash)
			if err != nil {
				errs = append(errs, fmt.Errorf("peer %s object %s: %w", url, hash, err))
				continue
			}
			added, err := e.store.Put(hash, data)
			if err != nil {
				errs = append(errs, fmt.Errorf("peer %s object %s: %w", url, hash, err))
				if errors.Is(err, ErrFull) {
					return result, errors.Join(errs...)
				}
				continue
			}
			if added {
				result.Added++
			}
		}
	}
	return result, errors.Join(errs...)
}
