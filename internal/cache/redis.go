package cache

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Jai-Keshav-Sharma/gatewai/internal/schema"
	"github.com/redis/go-redis/v9"
)

// redisKeyPrefix namespaces cache keys so they never collide with rate-limit
// keys or other gateway state in the same Redis instance.
const redisKeyPrefix = "gw:cache:"

// RedisCache is the distributed cache: responses live in Redis, so every
// gateway instance shares the same cache (scalability: shared state).
type RedisCache struct {
	client *redis.Client
}

// NewRedis builds a Redis-backed cache.
func NewRedis(client *redis.Client) *RedisCache {
	return &RedisCache{client: client}
}

// Get retrieves a response, unmarshaling it from JSON.
func (c *RedisCache) Get(ctx context.Context, key string) (*schema.UnifiedResponse, bool) {
	data, err := c.client.Get(ctx, redisKeyPrefix+key).Bytes()
	if err != nil {
		return nil, false // not found or connection error — treat as a miss
	}
	var ur schema.UnifiedResponse
	if err := json.Unmarshal(data, &ur); err != nil {
		return nil, false
	}
	return &ur, true
}

// Set stores a response as JSON with the TTL (Redis expires it server-side).
func (c *RedisCache) Set(ctx context.Context, key string, resp *schema.UnifiedResponse, ttl time.Duration) error {
	data, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	return c.client.Set(ctx, redisKeyPrefix+key, data, ttl).Err()
}

// Close closes the underlying connection pool.
func (c *RedisCache) Close() error {
	return c.client.Close()
}
