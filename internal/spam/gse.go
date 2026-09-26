package spam

import (
	"sync"

	"github.com/go-ego/gse"
)

// gseSegmenter adapts go-ego/gse, a pure-Go implementation of jieba whose
// dictionaries are embedded in the binary. Pure Go matters here: the service
// ships with CGO_ENABLED=0, so a cgo segmenter (gojieba) cannot be linked in.
type gseSegmenter struct {
	// gse does not document its cutter as concurrency-safe and its lookups walk
	// shared trie/hmm state, so cuts are serialized rather than assumed safe.
	// Cutting one message body is sub-millisecond at the sizes mail reaches,
	// and only CJK-bearing messages take this path at all.
	mu  sync.Mutex
	seg *gse.Segmenter
}

// NewChineseSegmenter loads the embedded Simplified Chinese dictionary and
// returns a segmenter for it. Loading is a one-time cost of a few megabytes of
// resident memory.
func NewChineseSegmenter() (Segmenter, error) {
	seg, err := gse.NewEmbed("zh_s")
	if err != nil {
		return nil, err
	}
	return &gseSegmenter{seg: &seg}, nil
}

// Cut splits text into dictionary words, using the DAG path plus HMM
// new-word discovery so unseen spam coinages decompose instead of vanishing.
func (g *gseSegmenter) Cut(text string) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.seg.Cut(text, true)
}
