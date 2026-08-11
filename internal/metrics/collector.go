package metrics

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"
)

// RecordRequest updates the request counter and duration histogram.
func RecordRequest(provider, model, statusCode string, cacheHit bool, duration time.Duration) {
	RequestsTotal.WithLabelValues(provider, model, statusCode, strconv.FormatBool(cacheHit)).Inc()
	RequestDuration.WithLabelValues(provider, model, strconv.FormatBool(cacheHit)).Observe(duration.Seconds())
}

// RecordTokens updates the token counters for a request.
func RecordTokens(provider, model string, tokensIn, tokensOut int) {
	if tokensIn > 0 {
		TokensTotal.WithLabelValues(provider, model, "input").Add(float64(tokensIn))
	}
	if tokensOut > 0 {
		TokensTotal.WithLabelValues(provider, model, "output").Add(float64(tokensOut))
	}
}

// RecordCost updates the cost counter for a request.
// Prices are USD per 1M tokens; tokens are integers, so cost per request is
// tiny — the counter accumulates it into meaningful spend over time.
func RecordCost(provider, model string, tokensIn, tokensOut int, inputPer1M, outputPer1M float64) {
	cost := float64(tokensIn)/1e6*inputPer1M + float64(tokensOut)/1e6*outputPer1M
	if cost > 0 {
		CostUSDTotal.WithLabelValues(provider, model).Add(cost)
	}
}

// RecordKey increments the per-key counter, hashing the key so key material
// never lands in a Prometheus label (§9 cardinality/security rule).
func RecordKey(virtualKey string) {
	sum := sha256.Sum256([]byte(virtualKey))
	KeyRequestsTotal.WithLabelValues(hex.EncodeToString(sum[:])).Inc()
}
