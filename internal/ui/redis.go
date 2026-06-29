package ui

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisReader is the subset of Redis operations the UI needs (read-only).
type RedisReader interface {
	// CurrentUsage returns the current request and token counters and their TTL
	// for the given consumer/service/user-type combination.
	// TTL is in seconds; -1 means no TTL (key without expiry).
	CurrentUsage(ctx context.Context, consumer, serviceType, userType string) (requests, tokens int64, ttlSeconds int64, err error)

	// QueueDepth returns LLEN relay:{model}:pending.
	QueueDepth(ctx context.Context, model string) (int64, error)
}

// redisReaderImpl wraps a go-redis client to satisfy RedisReader.
type redisReaderImpl struct {
	c *redis.Client
}

// NewRedisReader wraps a raw go-redis client for read-only UI operations.
func NewRedisReader(c *redis.Client) RedisReader {
	return &redisReaderImpl{c: c}
}

func (r *redisReaderImpl) CurrentUsage(ctx context.Context, consumer, serviceType, userType string) (requests, tokens int64, ttlSeconds int64, err error) {
	rlKey := "rl:" + consumer + ":" + serviceType + ":" + userType
	trlKey := "trl:" + consumer + ":" + serviceType + ":" + userType

	pipe := r.c.Pipeline()
	rlGet := pipe.Get(ctx, rlKey)
	trlGet := pipe.Get(ctx, trlKey)
	rlTTL := pipe.TTL(ctx, rlKey)
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return 0, 0, 0, err
	}

	if v, err2 := rlGet.Result(); err2 == nil {
		requests, _ = strconv.ParseInt(v, 10, 64)
	}
	if v, err2 := trlGet.Result(); err2 == nil {
		tokens, _ = strconv.ParseInt(v, 10, 64)
	}
	if ttl, err2 := rlTTL.Result(); err2 == nil {
		ttlSeconds = int64(ttl / time.Second)
	}
	return requests, tokens, ttlSeconds, nil
}

func (r *redisReaderImpl) QueueDepth(ctx context.Context, model string) (int64, error) {
	return r.c.LLen(ctx, "relay:"+model+":pending").Result()
}
