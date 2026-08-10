package ratelimit

import (
	"context"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisSlidingWindow is the Lua script implementing a fixed-window rate
// limit atomically on the Redis side. Atomicity matters: without it, two
// concurrent requests could both read the same count and both pass, breaking
// the limit under load. Lua scripts run atomically in Redis.
//
// Each request is recorded as a member of a sorted set (score = timestamp).
// Stale members outside the 60s window are pruned in the same atomic step.
const redisSlidingWindow = `
local key       = KEYS[1]
local now       = tonumber(ARGV[1])
local window    = tonumber(ARGV[2])
local limit     = tonumber(ARGV[3])
local cost      = tonumber(ARGV[4])

redis.call('ZREMRANGEBYSCORE', key, 0, now - window)
local count = redis.call('ZCARD', key)
if count + cost > limit then
    return {0, limit - count}
end
for i = 1, cost do
    redis.call('ZADD', key, now, now .. ':' .. math.random() .. ':' .. i)
end
redis.call('EXPIRE', key, window)
return {1, limit - count - cost}
`

// RedisLimiter is the distributed implementation: state lives in Redis, so
// every gateway instance shares ONE view of the limits. This is what makes
// rate limiting correct when the gateway is scaled horizontally (§3.2:
// Redis-backed state shared across instances).
type RedisLimiter struct {
	client     *redis.Client
	limits     map[string]int
	defaultRPM int
	defaultTPM int
	window     time.Duration
}

// NewRedis builds a Redis limiter. limits maps dimension → tokens per window.
func NewRedis(client *redis.Client, limits map[string]int, defaultRPM, defaultTPM int, window time.Duration) *RedisLimiter {
	return &RedisLimiter{client: client, limits: limits, defaultRPM: defaultRPM, defaultTPM: defaultTPM, window: window}
}

// Allow runs the sliding-window script for the dimension.
func (l *RedisLimiter) Allow(ctx context.Context, dimension string, cost int) (bool, int, error) {
	limit, ok := l.limits[dimension]
	if !ok {
		if strings.HasSuffix(dimension, ":tpm") {
			limit = l.defaultTPM
		} else {
			limit = l.defaultRPM
		}
		if limit <= 0 {
			return true, 0, nil
		}
	}
	res, err := l.client.Eval(ctx, redisSlidingWindow,
		[]string{keyPrefix + dimension},
		time.Now().UnixMilli(), l.window.Milliseconds(), limit, cost,
	).Int64Slice()
	if err != nil {
		return false, 0, err
	}
	return res[0] == 1, int(res[1]), nil
}

const keyPrefix = "gw:ratelimit:"
