package spam

import "testing"

// TestRedisKey pins the literal key construction so a namespace change is
// caught in the unit suite (the Redis client calls themselves are one file
// and reviewed; store behavior is exercised through memStore).
func TestRedisKey(t *testing.T) {
	if got := RedisKey(DefaultRedisPrefix, 123); got != "elecpostal:spam:bayes:123" {
		t.Fatalf("RedisKey = %q, want %q", got, "elecpostal:spam:bayes:123")
	}
}
