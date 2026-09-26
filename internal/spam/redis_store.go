package spam

import (
	"context"
	"fmt"
	"strconv"

	"github.com/redis/go-redis/v9"
)

// DefaultRedisPrefix is the key namespace for the Bayes model. App wiring
// passes it to NewRedisStore.
const DefaultRedisPrefix = "elecpostal:spam:bayes"

// redisBatchSize keeps each pipeline to a bounded number of keys.
const redisBatchSize = 128

type redisStore struct {
	client *redis.Client
	prefix string
}

// NewRedisStore wraps a go-redis client. prefix is the key namespace (e.g.
// DefaultRedisPrefix); the store derives token, meta and learned keys under
// it.
func NewRedisStore(client *redis.Client, prefix string) Store {
	return &redisStore{client: client, prefix: prefix}
}

// RedisKey returns the token key for a hash under prefix.
func RedisKey(prefix string, hash uint64) string {
	return fmt.Sprintf("%s:%d", prefix, hash)
}

func (r *redisStore) tokenKey(hash uint64) string     { return RedisKey(r.prefix, hash) }
func (r *redisStore) metaKey() string                 { return r.prefix + ":meta" }
func (r *redisStore) learnedKey(digest string) string { return r.prefix + ":learned:" + digest }

func (r *redisStore) GetCounts(ctx context.Context, hashes []uint64) ([]TokenCounts, error) {
	out := make([]TokenCounts, len(hashes))
	for start := 0; start < len(hashes); start += redisBatchSize {
		end := min(start+redisBatchSize, len(hashes))
		pipe := r.client.Pipeline()
		cmds := make([]*redis.SliceCmd, 0, end-start)
		for _, h := range hashes[start:end] {
			cmds = append(cmds, pipe.HMGet(ctx, r.tokenKey(h), "S", "H"))
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return nil, err
		}
		for i, cmd := range cmds {
			vals, err := cmd.Result()
			if err != nil {
				return nil, err
			}
			out[start+i] = TokenCounts{Spam: floatField(vals[0]), Ham: floatField(vals[1])}
		}
	}
	return out, nil
}

func (r *redisStore) Incr(ctx context.Context, hashes []uint64, class string, delta float64) error {
	field, metaField := classFields(class)
	for start := 0; start < len(hashes); start += redisBatchSize {
		end := min(start+redisBatchSize, len(hashes))
		pipe := r.client.Pipeline()
		keys := make([]string, 0, end-start+1)
		fields := make([]string, 0, end-start+1)
		for _, h := range hashes[start:end] {
			key := r.tokenKey(h)
			pipe.HIncrByFloat(ctx, key, field, delta)
			keys = append(keys, key)
			fields = append(fields, field)
		}
		pipe.HIncrByFloat(ctx, r.metaKey(), metaField, delta)
		keys = append(keys, r.metaKey())
		fields = append(fields, metaField)
		if _, err := pipe.Exec(ctx); err != nil {
			return err
		}
		if err := r.clampFields(ctx, keys, fields); err != nil {
			return err
		}
	}
	return nil
}

// clampFields floors any field that went negative (unlearning more than was
// learned) at 0.
func (r *redisStore) clampFields(ctx context.Context, keys, fields []string) error {
	pipe := r.client.Pipeline()
	cmds := make([]*redis.StringCmd, 0, len(keys))
	for i := range keys {
		cmds = append(cmds, pipe.HGet(ctx, keys[i], fields[i]))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return err
	}
	pipe2 := r.client.Pipeline()
	fixed := false
	for i, cmd := range cmds {
		v, err := cmd.Float64()
		if err == redis.Nil || v < 0 {
			pipe2.HSet(ctx, keys[i], fields[i], 0)
			fixed = true
		}
	}
	if !fixed {
		return nil
	}
	_, err := pipe2.Exec(ctx)
	return err
}

func (r *redisStore) LearnTotals(ctx context.Context) (float64, float64, error) {
	vals, err := r.client.HMGet(ctx, r.metaKey(), "learns_spam", "learns_ham").Result()
	if err != nil {
		return 0, 0, err
	}
	return floatField(vals[0]), floatField(vals[1]), nil
}

func (r *redisStore) MarkLearned(ctx context.Context, digest string, class string) (bool, error) {
	return r.client.SetNX(ctx, r.learnedKey(digest), class, 0).Result()
}

func (r *redisStore) IsLearned(ctx context.Context, digest string) (string, bool, error) {
	v, err := r.client.Get(ctx, r.learnedKey(digest)).Result()
	if err == redis.Nil {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

func (r *redisStore) DeleteLearned(ctx context.Context, digest string) error {
	return r.client.Del(ctx, r.learnedKey(digest)).Err()
}

func classFields(class string) (field, metaField string) {
	if class == "ham" {
		return "H", "learns_ham"
	}
	return "S", "learns_spam"
}

// floatField converts a Redis hash value (string) or nil to a float.
func floatField(v interface{}) float64 {
	switch x := v.(type) {
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	case float64:
		return x
	default:
		return 0
	}
}
