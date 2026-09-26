// Package spam implements the rspamd-style weighted symbol scoring and the
// Fisher/Robinson Bayes classifier used to route inbound mail to the Spam
// folder. Scoring never fails: any Bayes store error degrades to rules-only
// and the message still lands somewhere.
package spam

import (
	"context"
	"fmt"
	"math"

	"gorm.io/datatypes"
)

// Scorer scores a message and trains/unlearns the Bayes model. All methods
// must be safe to call concurrently.
type Scorer interface {
	Score(ctx context.Context, input Input) Result
	Learn(ctx context.Context, input Input, class string) error // class "spam" | "ham"
	Unlearn(ctx context.Context, input Input, class string) error
}

// Input is the message material the scorer needs. Authentication carries the
// SMTP-time SPF/DKIM/DMARC results produced by the senderauth verifier (may
// be empty or nil).
type Input struct {
	FromAddress    string
	FromName       string
	EnvelopeFrom   string
	Subject        string
	Body           string
	ContentType    string
	Authentication datatypes.JSON
	IsBlocked      bool // MailBlockRule match, computed by EmailService
}

// Symbol is a single scored rule hit.
type Symbol struct {
	Name   string  `json:"name"`
	Weight float64 `json:"weight"`
	Detail string  `json:"detail,omitempty"`
}

// Result is the accumulated score and the symbols that produced it. Score is
// the signed sum of symbol weights, floored at minScoreFloor.
type Result struct {
	Score   float64  `json:"score"`
	Symbols []Symbol `json:"symbols"`
}

// Config tunes the Bayes side of the scorer. The deterministic rules always
// run.
type Config struct {
	BayesEnabled bool
	MinLearns    int // per-class minimum learns before Bayes contributes
	MinTokens    int // minimum kept tokens before Bayes contributes
}

// Service implements Scorer. A nil store disables Bayes (rules only).
type Service struct {
	cfg   Config
	store Store
}

// NewService returns a scorer. Pass a nil store for a rules-only scorer.
func NewService(cfg Config, store Store) *Service {
	return &Service{cfg: cfg, store: store}
}

// Score evaluates the deterministic rules, then the Bayes classifier when
// enabled. It never returns an error; Bayes failures log and contribute zero.
func (s *Service) Score(ctx context.Context, input Input) Result {
	result := s.evaluateRules(input)
	if s.cfg.BayesEnabled && s.store != nil {
		p := s.classify(ctx, input)
		switch {
		case p > 0.6:
			w := bayesSpamWeight * math.Pow((p-0.5)*2, 8)
			result.Symbols = append(result.Symbols, Symbol{Name: SymbolBayesSpam, Weight: w})
			result.Score += w
		case p < 0.4:
			w := -bayesHamWeight * math.Pow((0.5-p)*2, 8)
			result.Symbols = append(result.Symbols, Symbol{Name: SymbolBayesHam, Weight: w})
			result.Score += w
		}
	}
	if result.Score < minScoreFloor {
		result.Score = minScoreFloor
	}
	return result
}

// Learn trains the Bayes model on input in class ("spam" or "ham"). It is
// idempotent per message digest and class: a second Learn of the same message
// is a no-op. A nil store is a no-op.
func (s *Service) Learn(ctx context.Context, input Input, class string) error {
	if s.store == nil {
		return nil
	}
	if err := validateClass(class); err != nil {
		return err
	}
	hashes := Tokenize(input)
	if len(hashes) == 0 {
		return nil
	}
	marked, err := s.store.MarkLearned(ctx, Digest(hashes), class)
	if err != nil {
		return err
	}
	if !marked {
		return nil // already learned in this class
	}
	return s.store.Incr(ctx, hashes, class, 1)
}

// Unlearn removes input from class. A nil store is a no-op.
func (s *Service) Unlearn(ctx context.Context, input Input, class string) error {
	if s.store == nil {
		return nil
	}
	if err := validateClass(class); err != nil {
		return err
	}
	hashes := Tokenize(input)
	if len(hashes) == 0 {
		return nil
	}
	if err := s.store.Incr(ctx, hashes, class, -1); err != nil {
		return err
	}
	return s.store.DeleteLearned(ctx, Digest(hashes))
}

func validateClass(class string) error {
	if class != "spam" && class != "ham" {
		return fmt.Errorf("spam: invalid class %q", class)
	}
	return nil
}
