// Package store provides Redis-backed job persistence for the relay.
// It fetches job records written by the gateway and atomically updates their
// result after inference completes.
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"gatewai/relay/internal/model"
)

const defaultTTLSecs = 259200 // 72 hours

// Store wraps a Redis client with job-specific helpers.
type Store struct {
	rdb *redis.Client
}

// New creates a Store backed by rdb.
func New(rdb *redis.Client) *Store {
	return &Store{rdb: rdb}
}

func jobKey(id string) string { return "job:" + id }

// GetJob fetches the job JSON stored at job:{id} and unmarshals the fields
// the relay needs to run the inference pipeline.
func (s *Store) GetJob(ctx context.Context, id string) (*model.Job, error) {
	data, err := s.rdb.Get(ctx, jobKey(id)).Bytes()
	if err == redis.Nil {
		return nil, fmt.Errorf("job %q not found in redis", id)
	}
	if err != nil {
		return nil, fmt.Errorf("getting job %q: %w", id, err)
	}

	var job model.Job
	if err := json.Unmarshal(data, &job); err != nil {
		return nil, fmt.Errorf("unmarshaling job %q: %w", id, err)
	}
	return &job, nil
}

// updateJobScript atomically reads a job JSON, skips already-terminal jobs
// (completed/failed), patches status/result_ref/error/updated_at, and
// re-writes the blob with the same TTL (falling back to 72 h if none is set).
//
// KEYS[1] = job:{id}
// ARGV[1] = status, ARGV[2] = result_ref, ARGV[3] = error, ARGV[4] = updated_at
var updateJobScript = redis.NewScript(`
local data = redis.call('GET', KEYS[1])
if not data then
    return redis.error_reply('job not found: ' .. KEYS[1])
end
local job = cjson.decode(data)
if job['status'] == 'completed' or job['status'] == 'failed' or job['status'] == 'cancelled' then
    return redis.status_reply('OK')
end
job['status']     = ARGV[1]
job['result_ref'] = ARGV[2]
job['error']      = ARGV[3]
job['updated_at'] = ARGV[4]
local ttl = tonumber(redis.call('TTL', KEYS[1]))
if ttl <= 0 then
    ttl = ` + fmt.Sprintf("%d", defaultTTLSecs) + `
end
redis.call('SET', KEYS[1], cjson.encode(job), 'EX', ttl)
return redis.status_reply('OK')
`)

// UpdateJobResult atomically patches the job record's status, result_ref, and
// error fields using a Lua script that preserves the existing TTL.
// Terminal jobs (completed/failed) are silently skipped.
func (s *Store) UpdateJobResult(ctx context.Context, id string, status model.JobStatus, resultRef, errMsg string) error {
	updatedAt := time.Now().UTC().Format(time.RFC3339Nano)
	err := updateJobScript.Run(ctx, s.rdb,
		[]string{jobKey(id)},
		string(status), resultRef, errMsg, updatedAt,
	).Err()
	if err != nil {
		return fmt.Errorf("updating job %q: %w", id, err)
	}
	return nil
}
