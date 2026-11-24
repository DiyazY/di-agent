package gossip

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Client retrieves exact wire bytes; redirects are refused so a registered
// endpoint cannot redirect the exchange to an unrelated destination.
type Client struct {
	http *http.Client
}

func NewClient() *Client {
	return &Client{http: &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

func (c *Client) Fetch(ctx context.Context, peerURL, hash string) ([]byte, error) {
	if err := ValidateHash(hash); err != nil {
		return nil, err
	}
	data, err := c.get(ctx, peerURL, "/gossip/objects/"+hash, MaxObjectBytes)
	if err != nil {
		return nil, err
	}
	if err := Verify(hash, data); err != nil {
		return nil, fmt.Errorf("gossip fetch: %w", err)
	}
	return data, nil
}

// Inventory is an advertisement, not authority: validate its shape and limits
// before using any hash to request content.
func (c *Client) Inventory(ctx context.Context, peerURL string) (Inventory, error) {
	data, err := c.get(ctx, peerURL, "/gossip/objects", MaxInventoryBytes)
	if err != nil {
		return Inventory{}, err
	}
	if len(data) > MaxInventoryBytes {
		return Inventory{}, fmt.Errorf("%w: response exceeds %d bytes", ErrInvalidInventory, MaxInventoryBytes)
	}
	var out Inventory
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return Inventory{}, fmt.Errorf("%w: %v", ErrInvalidInventory, err)
	}
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return Inventory{}, fmt.Errorf("%w: expected one JSON inventory", ErrInvalidInventory)
	}
	if out.Hashes == nil || len(out.Hashes) > MaxObjects ||
		out.Bytes < 0 || out.Bytes > MaxStoreBytes ||
		(len(out.Hashes) == 0) != (out.Bytes == 0) {
		return Inventory{}, fmt.Errorf("%w: hashes or byte count outside store limits", ErrInvalidInventory)
	}
	seen := make(map[string]bool, len(out.Hashes))
	for _, hash := range out.Hashes {
		if err := ValidateHash(hash); err != nil {
			return Inventory{}, fmt.Errorf("%w: %v", ErrInvalidInventory, err)
		}
		if seen[hash] {
			return Inventory{}, fmt.Errorf("%w: duplicate hash %s", ErrInvalidInventory, hash)
		}
		seen[hash] = true
	}
	slices.Sort(out.Hashes)
	return out, nil
}

func (c *Client) get(ctx context.Context, peerURL, path string, limit int64) ([]byte, error) {
	u, err := url.Parse(peerURL)
	if err != nil {
		return nil, fmt.Errorf("gossip peer URL: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("gossip peer URL must be an HTTP(S) endpoint without credentials, query or fragment")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(peerURL, "/")+path, nil)
	if err != nil {
		return nil, fmt.Errorf("gossip fetch: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gossip fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gossip fetch: peer returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("gossip fetch: reading object: %w", err)
	}
	return data, nil
}
