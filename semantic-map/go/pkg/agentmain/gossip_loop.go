package agentmain

import (
	"context"
	"log"
	"math/rand/v2"
	"time"

	"github.com/DiyazY/di-agent/pkg/gossip"
	"github.com/DiyazY/di-agent/pkg/peers"
)

// startGossipLoop returns a completion signal so shutdown can wait for the last
// in-flight exchange. Zero interval preserves manual-only mode.
func startGossipLoop(ctx context.Context, exchange *gossip.Exchange, registry *peers.Registry,
	every time.Duration, membership *gossip.Membership) <-chan struct{} {
	if exchange == nil || every <= 0 {
		return nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		lastError := ""
		for {
			if ctx.Err() != nil {
				return
			}
			var urls, known []string
			var err error
			if membership != nil {
				urls, known = membership.SelectActive()
			} else {
				var descriptors []*peers.Descriptor
				descriptors, err = registry.List()
				for _, peer := range descriptors {
					urls = append(urls, peer.URL)
				}
				known = urls
			}
			var result gossip.RoundResult
			if err == nil {
				result, err = exchange.RoundWithKnownPeers(ctx, urls, known)
			}
			if ctx.Err() != nil {
				return
			}
			if result.Added > 0 {
				log.Printf("gossip: received %d objects from %d peer inventories", result.Added, result.Peers)
			}
			if err != nil {
				if message := err.Error(); message != lastError {
					log.Printf("gossip: exchange failed: %s", message)
					lastError = message
				}
			} else if lastError != "" {
				log.Printf("gossip: exchange recovered")
				lastError = ""
			}
			// Positive jitter avoids synchronised polling and never polls faster
			// than the configured interval.
			delay := every + time.Duration(rand.Int64N(max(1, int64(every/10))))
			if delay < every {
				delay = every
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return done
}
