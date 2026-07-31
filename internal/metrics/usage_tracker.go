package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
)

// StartUsageTopNRefresh launches a background goroutine that reads the top-N
// consumers per service type from usage:consumer:{svcType}:tokens:{prompt|completion}
// Redis sorted sets every refreshInterval and updates UsageTokensTop. The
// goroutine stops when ctx is cancelled. No-op when topN <= 0 or serviceTypes
// is empty.
func StartUsageTopNRefresh(ctx context.Context, client *redis.Client, topN int, refreshInterval time.Duration, serviceTypes []string) {
	if topN <= 0 || len(serviceTypes) == 0 {
		return
	}
	go func() {
		refreshUsageTopN(ctx, client, topN, serviceTypes) // populate on startup without waiting one full interval
		ticker := time.NewTicker(refreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refreshUsageTopN(ctx, client, topN, serviceTypes)
			}
		}
	}()
}

func refreshUsageTopN(ctx context.Context, client *redis.Client, topN int, serviceTypes []string) {
	tokenTypes := []string{"prompt", "completion"}

	UsageTokensTop.Reset()
	UsageRequestsTop.Reset()
	UsageProcessingTimeTop.Reset()

	for _, svcType := range serviceTypes {
		for _, tt := range tokenTypes {
			key := fmt.Sprintf("usage:consumer:%s:tokens:%s", svcType, tt)
			results, err := client.ZRevRangeWithScores(ctx, key, 0, int64(topN-1)).Result()
			if err != nil {
				slog.WarnContext(ctx, "usage tracker: top-N refresh failed", "key", key, "error", err)
				continue
			}
			for _, z := range results {
				consumer, ok := z.Member.(string)
				if !ok {
					continue
				}
				UsageTokensTop.WithLabelValues(consumer, svcType, tt).Set(z.Score)
			}
		}

		refreshUsageScalarTopN(ctx, client, topN, svcType, "requests", UsageRequestsTop)
		refreshUsageScalarTopN(ctx, client, topN, svcType, "processing_time", UsageProcessingTimeTop)
	}
}

// refreshUsageScalarTopN reads the top-N members of usage:consumer:<svcType>:<metric>
// and sets them on a {consumer, service_type} GaugeVec.
func refreshUsageScalarTopN(ctx context.Context, client *redis.Client, topN int, svcType, metric string, gauge *prometheus.GaugeVec) {
	key := fmt.Sprintf("usage:consumer:%s:%s", svcType, metric)
	results, err := client.ZRevRangeWithScores(ctx, key, 0, int64(topN-1)).Result()
	if err != nil {
		slog.WarnContext(ctx, "usage tracker: top-N refresh failed", "key", key, "error", err)
		return
	}
	for _, z := range results {
		consumer, ok := z.Member.(string)
		if !ok {
			continue
		}
		gauge.WithLabelValues(consumer, svcType).Set(z.Score)
	}
}
