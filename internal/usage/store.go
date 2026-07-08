package usage

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// UsageStore reads per-consumer usage from Redis.
type UsageStore interface {
	GetConsumerUsage(ctx context.Context, consumer, userType string, serviceTypes []string) (*ConsumerUsage, error)
	ListConsumers(ctx context.Context, limit, offset int64) (consumers []string, total int64, err error)
	ListConsumersByType(ctx context.Context, serviceType string, limit, offset int64) (consumers []string, total int64, err error)
}

type redisUsageStore struct {
	rdb       *redis.Client
	retention string // display label only ("all-time" when empty)
}

// NewRedisUsageStore returns a UsageStore backed by Redis.
// retention is the human-readable retention string surfaced in API responses ("" = "all-time").
func NewRedisUsageStore(rdb *redis.Client, retention string) UsageStore {
	return &redisUsageStore{rdb: rdb, retention: retention}
}

func (s *redisUsageStore) retentionLabel() string {
	if s.retention == "" {
		return "all-time"
	}
	return s.retention
}

// GetConsumerUsage returns cumulative + window usage for consumer across the
// given service types. Service types with zero data are omitted.
func (s *redisUsageStore) GetConsumerUsage(ctx context.Context, consumer, userType string, serviceTypes []string) (*ConsumerUsage, error) {
	out := &ConsumerUsage{
		Consumer:  consumer,
		Retention: s.retentionLabel(),
	}

	// last_active from usage:consumers
	score, err := s.rdb.ZScore(ctx, "usage:consumers", consumer).Result()
	if err == nil {
		t := time.Unix(int64(score), 0).UTC()
		out.LastActive = &t
	}

	for _, svcType := range serviceTypes {
		svc, err := s.getServiceUsage(ctx, consumer, userType, svcType)
		if err != nil {
			slog.WarnContext(ctx, "usage store: failed to read service usage",
				"consumer", consumer, "service_type", svcType, "error", err)
			continue
		}
		if svc == nil {
			continue // no data for this service type
		}
		out.Usage = append(out.Usage, *svc)
	}
	return out, nil
}

func (s *redisUsageStore) getServiceUsage(ctx context.Context, consumer, userType, svcType string) (*ServiceUsage, error) {
	requests, _ := s.zscore(ctx, "usage:consumer:"+svcType+":requests", consumer)
	jobs, _ := s.zscore(ctx, "usage:consumer:"+svcType+":jobs", consumer)
	procTime, _ := s.zscoref(ctx, "usage:consumer:"+svcType+":processing_time", consumer)

	// LLM tokens come from the existing ConsumerTracker sorted sets.
	var tokens *TokenUsage
	if svcType == "llm" {
		prompt, _ := s.zscore(ctx, "llm:consumer:tokens:"+userType+":prompt", consumer)
		completion, _ := s.zscore(ctx, "llm:consumer:tokens:"+userType+":completion", consumer)
		if prompt > 0 || completion > 0 {
			tokens = &TokenUsage{Prompt: prompt, Completion: completion}
		}
	}

	// Generic (non-LLM-proxy) tokens come from the UsageTracker.TrackTokens sorted
	// sets. Only consulted when the "llm" branch above found nothing, so the two
	// key formats never both contribute to the same response.
	if tokens == nil {
		promptGeneric, _ := s.zscore(ctx, "usage:consumer:"+svcType+":tokens:prompt", consumer)
		completionGeneric, _ := s.zscore(ctx, "usage:consumer:"+svcType+":tokens:completion", consumer)
		if promptGeneric > 0 || completionGeneric > 0 {
			tokens = &TokenUsage{Prompt: promptGeneric, Completion: completionGeneric}
		}
	}

	// Skip service types with no recorded data.
	if requests == 0 && jobs == 0 && procTime == 0 && tokens == nil {
		return nil, nil
	}

	total := TotalUsage{
		Requests:       requests,
		Jobs:           jobs,
		ProcessingTime: procTime,
		Tokens:         tokens,
	}

	window := s.getWindowUsage(ctx, consumer, userType, svcType)

	return &ServiceUsage{
		ServiceType: svcType,
		Total:       total,
		Window:      window,
	}, nil
}

// getWindowUsage reads rate-limit window counters for this consumer+service.
// Returns nil when no window data exists.
func (s *redisUsageStore) getWindowUsage(ctx context.Context, consumer, userType, svcType string) *WindowUsage {
	rlKey := fmt.Sprintf("rl:%s:%s:%s", consumer, svcType, userType)
	trlKey := fmt.Sprintf("trl:%s:%s:%s", consumer, svcType, userType)
	ptrlKey := fmt.Sprintf("ptrl:%s:%s:%s", consumer, svcType, userType)

	rlVal, rlTTL := s.getIntWithTTL(ctx, rlKey)
	trlVal, _ := s.getIntWithTTL(ctx, trlKey)
	ptrlVal, _ := s.getIntWithTTL(ctx, ptrlKey)

	if rlVal == 0 && trlVal == 0 && ptrlVal == 0 {
		return nil
	}

	w := &WindowUsage{
		Requests:       rlVal,
		Tokens:         trlVal,
		ProcessingTime: float64(ptrlVal),
	}
	if rlTTL > 0 {
		resetAt := time.Now().Add(rlTTL).UTC()
		w.ResetAt = &resetAt
	}
	return w
}

// ListConsumers returns consumers sorted by most-recently-active (desc),
// paginated from usage:consumers.
func (s *redisUsageStore) ListConsumers(ctx context.Context, limit, offset int64) ([]string, int64, error) {
	total, _ := s.rdb.ZCard(ctx, "usage:consumers").Result()
	members, _ := s.rdb.ZRevRange(ctx, "usage:consumers", offset, offset+limit-1).Result()
	return members, total, nil
}

// ListConsumersByType returns consumers sorted by request count desc for the
// given service type, paginated from usage:consumer:{type}:requests.
func (s *redisUsageStore) ListConsumersByType(ctx context.Context, serviceType string, limit, offset int64) ([]string, int64, error) {
	key := "usage:consumer:" + serviceType + ":requests"
	total, _ := s.rdb.ZCard(ctx, key).Result()
	members, _ := s.rdb.ZRevRange(ctx, key, offset, offset+limit-1).Result()
	return members, total, nil
}

// helpers

func (s *redisUsageStore) zscore(ctx context.Context, key, member string) (int64, error) {
	f, err := s.rdb.ZScore(ctx, key, member).Result()
	if err != nil {
		return 0, err
	}
	return int64(f), nil
}

func (s *redisUsageStore) zscoref(ctx context.Context, key, member string) (float64, error) {
	return s.rdb.ZScore(ctx, key, member).Result()
}

func (s *redisUsageStore) getIntWithTTL(ctx context.Context, key string) (int64, time.Duration) {
	val, err := s.rdb.Get(ctx, key).Result()
	if err != nil {
		return 0, 0
	}
	n, _ := strconv.ParseInt(val, 10, 64)
	ttl, _ := s.rdb.TTL(ctx, key).Result()
	return n, ttl
}
