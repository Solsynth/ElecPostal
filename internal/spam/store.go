package spam

import (
	"context"
	"sync"
)

// Store persists the Bayes model. Token hashes are uint64 fnv-1a digests;
// classes are "spam" and "ham".
type Store interface {
	// GetCounts returns per-hash spam/ham counts, parallel to hashes.
	GetCounts(ctx context.Context, hashes []uint64) ([]TokenCounts, error)
	// Incr adjusts token counts and the class total by delta (may be
	// negative for unlearning). Counts floor at 0.
	Incr(ctx context.Context, hashes []uint64, class string, delta float64) error
	// LearnTotals returns the total spam and ham learning counts.
	LearnTotals(ctx context.Context) (spam, ham float64, err error)
	// MarkLearned marks digest as learned in class; true when newly marked.
	MarkLearned(ctx context.Context, digest string, class string) (bool, error)
	// IsLearned reports the class a digest was learned in, if any.
	IsLearned(ctx context.Context, digest string) (class string, learned bool, err error)
	// DeleteLearned removes the learned marker for digest.
	DeleteLearned(ctx context.Context, digest string) error
}

// TokenCounts holds the spam/ham occurrence counts of one token hash.
type TokenCounts struct {
	Spam float64
	Ham  float64
}

// memStore is an in-memory Store used by all tests.
type memStore struct {
	mu      sync.Mutex
	tokens  map[uint64]TokenCounts
	spam    float64
	ham     float64
	learned map[string]string // digest -> class
}

func newMemStore() *memStore {
	return &memStore{
		tokens:  make(map[uint64]TokenCounts),
		learned: make(map[string]string),
	}
}

func (m *memStore) GetCounts(ctx context.Context, hashes []uint64) ([]TokenCounts, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]TokenCounts, len(hashes))
	for i, h := range hashes {
		out[i] = m.tokens[h]
	}
	return out, nil
}

func (m *memStore) Incr(ctx context.Context, hashes []uint64, class string, delta float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, h := range hashes {
		tc := m.tokens[h]
		if class == "ham" {
			tc.Ham = max(0, tc.Ham+delta)
		} else {
			tc.Spam = max(0, tc.Spam+delta)
		}
		m.tokens[h] = tc
	}
	if class == "ham" {
		m.ham = max(0, m.ham+delta)
	} else {
		m.spam = max(0, m.spam+delta)
	}
	return nil
}

func (m *memStore) LearnTotals(ctx context.Context) (float64, float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.spam, m.ham, nil
}

func (m *memStore) MarkLearned(ctx context.Context, digest string, class string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.learned[digest]; ok {
		return false, nil
	}
	m.learned[digest] = class
	return true, nil
}

func (m *memStore) IsLearned(ctx context.Context, digest string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cls, ok := m.learned[digest]
	return cls, ok, nil
}

func (m *memStore) DeleteLearned(ctx context.Context, digest string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.learned, digest)
	return nil
}
