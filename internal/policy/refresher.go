package policy

import (
	"context"
	"log"
	"time"
)

// StartRuleRefresher launches a goroutine that re-pulls rules from store
// every interval and replaces the engine's cached snapshot. Returns when
// ctx is cancelled.
func StartRuleRefresher(ctx context.Context, engine *ChainEngine, store *RuleStore, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := engine.PullFromStore(ctx, store); err != nil {
					log.Printf("policy: rule refresh failed: %v", err)
				}
			}
		}
	}()
}
