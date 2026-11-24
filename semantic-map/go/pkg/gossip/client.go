package gossip

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	u, err := url.Parse(peerURL)
	if err != nil {
		return nil, fmt.Errorf("gossip peer URL: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("gossip peer URL must be an HTTP(S) endpoint without credentials, query or fragment")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(peerURL, "/")+"/gossip/objects/"+hash, nil)
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
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxObjectBytes+1))
	if err != nil {
		return nil, fmt.Errorf("gossip fetch: reading object: %w", err)
	}
	if err := Verify(hash, data); err != nil {
		return nil, fmt.Errorf("gossip fetch: %w", err)
	}
	return data, nil
}
