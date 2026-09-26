package spam

// Segmenter splits text written without word separators — Chinese, Japanese —
// into words. Implementations must be safe for concurrent use.
//
// A nil Segmenter is valid: the tokenizer then falls back to character
// bigrams, which need no dictionary and still approximate word boundaries well
// enough to classify. Supplying a segmenter uses dictionary words instead, at
// the cost of a dictionary dependency and a larger feature space.
type Segmenter interface {
	Cut(text string) []string
}
