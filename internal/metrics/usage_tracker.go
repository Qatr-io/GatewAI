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
		tokenResults := make(map[string][]redis.Z, len(tokenTypes))
		for _, tt := range tokenTypes {
			key := fmt.Sprintf("usage:consumer:%s:tokens:%s", svcType, tt)
			results, err := fetchTopN(ctx, client, key, topN)
			if err != nil {
				slog.WarnContext(ctx, "usage tracker: top-N refresh failed", "key", key, "error", err)
				continue
			}
			tokenResults[tt] = results
		}

		requestsResults, err := fetchTopN(ctx, client, fmt.Sprintf("usage:consumer:%s:requests", svcType), topN)
		if err != nil {
			slog.WarnContext(ctx, "usage tracker: top-N refresh failed", "svcType", svcType, "metric", "requests", "error", err)
		}
		processingResults, err := fetchTopN(ctx, client, fmt.Sprintf("usage:consumer:%s:processing_time", svcType), topN)
		if err != nil {
			slog.WarnContext(ctx, "usage tracker: top-N refresh failed", "svcType", svcType, "metric", "processing_time", "error", err)
		}

		consumers := make(map[string]struct{})
		for _, results := range tokenResults {
			collectConsumers(results, consumers)
		}
		collectConsumers(requestsResults, consumers)
		collectConsumers(processingResults, consumers)

		userTypes := fetchUserTypes(ctx, client, svcType, consumers)

		for tt, results := range tokenResults {
			for _, z := range results {
				consumer, ok := z.Member.(string)
				if !ok {
					continue
				}
				UsageTokensTop.WithLabelValues(consumer, svcType, tt, userTypes[consumer]).Set(z.Score)
			}
		}
		setScalarGauge(UsageRequestsTop, requestsResults, svcType, userTypes)
		setScalarGauge(UsageProcessingTimeTop, processingResults, svcType, userTypes)
	}
}

// fetchTopN reads the top-N members (by score) of a Redis sorted set.
func fetchTopN(ctx context.Context, client *redis.Client, key string, topN int) ([]redis.Z, error) {
	return client.ZRevRangeWithScores(ctx, key, 0, int64(topN-1)).Result()
}

// collectConsumers adds every sorted-set member in results to into.
func collectConsumers(results []redis.Z, into map[string]struct{}) {
	for _, z := range results {
		if consumer, ok := z.Member.(string); ok {
			into[consumer] = struct{}{}
		}
	}
}

// fetchUserTypes joins consumers against usage:consumer:{svcType}:usertype,
// the rate-limit tier a consumer was last evaluated under for that service
// (recorded by usage.UsageTracker.TrackUserType). Missing consumers are
// simply absent from the returned map, so callers see an empty user_type.
func fetchUserTypes(ctx context.Context, client *redis.Client, svcType string, consumers map[string]struct{}) map[string]string {
	result := make(map[string]string, len(consumers))
	if len(consumers) == 0 {
		return result
	}
	list := make([]string, 0, len(consumers))
	for consumer := range consumers {
		list = append(list, consumer)
	}
	key := "usage:consumer:" + svcType + ":usertype"
	vals, err := client.HMGet(ctx, key, list...).Result()
	if err != nil {
		slog.WarnContext(ctx, "usage tracker: user_type lookup failed", "key", key, "error", err)
		return result
	}
	for i, v := range vals {
		if s, ok := v.(string); ok && s != "" {
			result[list[i]] = s
		}
	}
	return result
}

// setScalarGauge sets a {consumer, service_type, user_type} GaugeVec from a
// sorted-set top-N result.
func setScalarGauge(gauge *prometheus.GaugeVec, results []redis.Z, svcType string, userTypes map[string]string) {
	for _, z := range results {
		consumer, ok := z.Member.(string)
		if !ok {
			continue
		}
		gauge.WithLabelValues(consumer, svcType, userTypes[consumer]).Set(z.Score)
	}
}
