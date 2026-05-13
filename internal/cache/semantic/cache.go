// Package semantic implements a semantic (vector-similarity) LLM response cache
// backed by Redis Stack (RediSearch + vector index).
//
// # Prerequisites
//
// Redis Stack must be available (redis/redis-stack image or OSS Redis ≥ 7.2 with
// the RediSearch module). The standard Redis-HA chart does not include RediSearch —
// verify your deployment before enabling this feature.
//
// # How it works
//
//  1. Extract text from OpenAI-compatible messages payload.
//  2. Obtain a float32 embedding vector via the configured embedding service.
//  3. Query the HNSW vector index for the nearest neighbour within the configured
//     cosine-similarity threshold.
//  4. On a hit, return the stored response with X-Cache: SEMANTIC-HIT.
//  5. On a miss, forward the request, then store the embedding + response.
//
// The index is created automatically on first use. If Redis Stack is unavailable
// (missing RediSearch module) every Get/Set returns immediately as a miss/noop.
package semantic

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"kevent/gateway/internal/cache"
)

const (
	indexName    = "kevent:semcache:idx"
	hashPrefix   = "kevent:semcache:"
	fieldVec     = "vec"
	fieldResp    = "resp"
	fieldStatus  = "status"
	fieldCT      = "ct"
)

// EmbedderFunc returns a float32 embedding vector for the given text.
// The returned slice length must equal the dimension configured in the cache.
type EmbedderFunc func(ctx context.Context, text string) ([]float32, error)

// Cache is a semantic similarity cache backed by Redis Stack.
// A nil *Cache is valid and acts as a no-op.
type Cache struct {
	rdb       *redis.Client
	embed     EmbedderFunc
	threshold float64  // minimum cosine similarity (0–1), e.g. 0.95
	dim       int      // embedding vector dimension, e.g. 1024 for bge-m3

	once       sync.Once
	indexReady bool // true after successful index creation or confirmation
}

// New creates a SemanticCache. threshold is the minimum cosine similarity
// for a cache hit (0–1). dim is the embedding vector dimension.
func New(rdb *redis.Client, embed EmbedderFunc, threshold float64, dim int) *Cache {
	return &Cache{
		rdb:       rdb,
		embed:     embed,
		threshold: threshold,
		dim:       dim,
	}
}

// Get looks up the nearest semantically-similar cached response.
// Returns (entry, true, nil) on a semantic hit, (nil, false, nil) on a miss.
// Returns (nil, false, err) only for unexpected errors — callers should treat
// these as cache misses and continue.
func (c *Cache) Get(ctx context.Context, body []byte) (*cache.Entry, bool, error) {
	if c == nil {
		return nil, false, nil
	}
	c.ensureIndex(ctx)
	if !c.indexReady {
		return nil, false, nil
	}

	text := extractText(body)
	if text == "" {
		return nil, false, nil
	}

	vec, err := c.embed(ctx, text)
	if err != nil {
		return nil, false, fmt.Errorf("semantic cache embed: %w", err)
	}

	entry, hit, err := c.search(ctx, vec)
	return entry, hit, err
}

// Set stores the embedding for body alongside the response entry.
func (c *Cache) Set(ctx context.Context, body []byte, entry *cache.Entry, ttl time.Duration) error {
	if c == nil {
		return nil
	}
	c.ensureIndex(ctx)
	if !c.indexReady {
		return nil
	}

	text := extractText(body)
	if text == "" {
		return nil
	}

	vec, err := c.embed(ctx, text)
	if err != nil {
		return fmt.Errorf("semantic cache embed for set: %w", err)
	}

	return c.store(ctx, body, vec, entry, ttl)
}

// ensureIndex creates the RediSearch vector index if it does not already exist.
// It runs at most once per Cache instance.
func (c *Cache) ensureIndex(ctx context.Context) {
	c.once.Do(func() {
		err := c.rdb.Do(ctx,
			"FT.CREATE", indexName,
			"ON", "HASH",
			"PREFIX", "1", hashPrefix,
			"SCHEMA",
			fieldVec, "VECTOR", "HNSW", "6",
			"TYPE", "FLOAT32",
			"DIM", fmt.Sprint(c.dim),
			"DISTANCE_METRIC", "COSINE",
			fieldResp, "TEXT",
			fieldStatus, "NUMERIC",
			fieldCT, "TEXT",
		).Err()

		if err != nil {
			// "Index already exists" is not a real error — the index was created in
			// a previous process. Treat as success.
			if strings.Contains(err.Error(), "Index already exists") {
				c.indexReady = true
				return
			}
			slog.Warn("semantic cache: failed to create vector index (Redis Stack required)",
				"error", err)
			return
		}
		c.indexReady = true
	})
}

// search queries the HNSW index for the nearest neighbour of vec.
// Returns (entry, true, nil) when similarity ≥ threshold.
func (c *Cache) search(ctx context.Context, vec []float32) (*cache.Entry, bool, error) {
	vecBytes := encodeFloat32(vec)

	// KNN=1: retrieve the single nearest neighbour along with its cosine distance.
	res, err := c.rdb.Do(ctx,
		"FT.SEARCH", indexName,
		"*=>[KNN 1 @"+fieldVec+" $qvec AS __score]",
		"PARAMS", "2", "qvec", vecBytes,
		"RETURN", "4", fieldResp, fieldStatus, fieldCT, "__score",
		"SORTBY", "__score",
		"DIALECT", "2",
	).Slice()
	if err != nil {
		return nil, false, fmt.Errorf("semantic cache search: %w", err)
	}

	entry, score, found := parseSearchResult(res)
	if !found {
		return nil, false, nil
	}

	// RediSearch COSINE distance = 1 − cosine_similarity.
	similarity := 1.0 - score
	if similarity < c.threshold {
		return nil, false, nil
	}
	return entry, true, nil
}

// store saves the embedding and response in a Redis HASH.
func (c *Cache) store(ctx context.Context, body []byte, vec []float32, entry *cache.Entry, ttl time.Duration) error {
	// Use a SHA-256 of the body as a stable key to deduplicate identical requests.
	h := sha256.Sum256(body)
	key := fmt.Sprintf("%s%x", hashPrefix, h[:8])

	respJSON, err := json.Marshal(entry.Body)
	if err != nil {
		return fmt.Errorf("semantic cache marshal response: %w", err)
	}

	pipe := c.rdb.Pipeline()
	pipe.HSet(ctx, key,
		fieldVec, encodeFloat32(vec),
		fieldResp, string(respJSON),
		fieldStatus, entry.StatusCode,
		fieldCT, entry.ContentType,
	)
	if ttl > 0 {
		pipe.Expire(ctx, key, ttl)
	}
	_, err = pipe.Exec(ctx)
	return err
}

// parseSearchResult extracts the first document from an FT.SEARCH result.
// FT.SEARCH response layout: [total, key, [field, val, ...], ...]
func parseSearchResult(res []interface{}) (*cache.Entry, float64, bool) {
	if len(res) < 3 {
		return nil, 0, false
	}
	// res[0] = total hits (int64)
	// res[1] = first document key
	// res[2] = field-value list for first document
	fields, ok := res[2].([]interface{})
	if !ok || len(fields) < 2 {
		return nil, 0, false
	}

	fieldMap := make(map[string]string, len(fields)/2)
	for i := 0; i+1 < len(fields); i += 2 {
		k, _ := fields[i].(string)
		v, _ := fields[i+1].(string)
		fieldMap[k] = v
	}

	respRaw := fieldMap[fieldResp]
	if respRaw == "" {
		return nil, 0, false
	}
	var body []byte
	if err := json.Unmarshal([]byte(respRaw), &body); err != nil {
		return nil, 0, false
	}

	var status int
	fmt.Sscanf(fieldMap[fieldStatus], "%d", &status)
	if status == 0 {
		status = 200
	}

	score := 0.0
	fmt.Sscanf(fieldMap["__score"], "%f", &score)

	return &cache.Entry{
		Body:        body,
		ContentType: fieldMap[fieldCT],
		StatusCode:  status,
	}, score, true
}

// encodeFloat32 packs a float32 slice into a little-endian byte slice
// suitable for storing as a Redis VECTOR field.
func encodeFloat32(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

// extractText concatenates the message content fields from an OpenAI-compatible
// JSON payload into a single string for embedding.
func extractText(body []byte) string {
	var payload struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || len(payload.Messages) == 0 {
		return ""
	}

	var parts []string
	for _, msg := range payload.Messages {
		if len(msg.Content) == 0 {
			continue
		}
		var s string
		if err := json.Unmarshal(msg.Content, &s); err == nil {
			parts = append(parts, s)
			continue
		}
		var contentParts []struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(msg.Content, &contentParts); err == nil {
			for _, p := range contentParts {
				if p.Text != "" {
					parts = append(parts, p.Text)
				}
			}
		}
	}
	return strings.Join(parts, "\n")
}
