package spam

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// Corpus generators. Lexicons are deliberately disjoint so held-out Bayes
// probabilities are extreme (spam ~1.0, ham ~0) and the corpus test is
// robust. Seeds above 1000 are held out (never trained).

var spamLexicon = []string{
	"viagra", "free", "prize", "winner", "claim", "cash", "bonus", "urgent",
	"guaranteed", "discount", "cheap", "limited", "exclusive", "selected",
	"congratulations", "instant", "credit", "loan", "pharmacy", "pills",
	"prescription", "enlarge", "weight", "loss", "income", "opportunity",
	"investment", "profit", "millions", "dollars", "transfer", "wire",
	"verify", "alert", "password", "suspended", "unlock", "lottery",
	"jackpot", "casino", "betting", "poker", "dating", "herbal", "remedy",
	"cure", "vitamins", "supplements", "beauty", "special", "offer", "deal",
	"sale", "purchase", "buy", "act", "today", "secret", "miracle",
	"amazing", "incredible", "hurry", "expires", "reserve", "premium",
	"deluxe", "ultimate", "revolutionary", "breakthrough",
}

var hamLexicon = []string{
	"meeting", "agenda", "minutes", "project", "status", "report",
	"quarterly", "revenue", "client", "feedback", "schedule", "tomorrow",
	"attendance", "lunch", "coffee", "design", "review", "document",
	"attachment", "thanks", "regards", "team", "friday", "monday", "notes",
	"discuss", "proposal", "budget", "hiring", "interview", "candidate",
	"onboarding", "invoice", "receipt", "shipping", "tracking", "delivery",
	"subscription", "weekly", "digest", "archive", "preferences", "roadmap",
	"sprint", "retro", "standup", "deploy", "staging", "production",
	"database", "migration", "endpoint", "service", "pipeline", "sync",
	"summary", "action", "items", "owner", "deadline", "milestone",
	"release", "version", "hotfix", "bugfix", "feature", "prototype",
	"wireframe", "mockup", "research", "findings",
}

func pickWords(lex []string, seed, n int) []string {
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = lex[(seed*17+i*23)%len(lex)]
	}
	return out
}

// spamMessage builds a realistic spammy message: all-caps subject and body,
// money phrases, exclamations, six URLs and a spoofed "Support" display name.
// Deterministic per seed; held-out seeds share the trained lexicon.
func spamMessage(seed int) Input {
	words := pickWords(spamLexicon, seed, 18)
	subject := strings.ToUpper(strings.Join(words[:3], " ") + "!!!")
	body := strings.ToUpper(strings.Join(words[3:], " "))
	body += " CLICK HERE TO CLAIM https://spam.example/claim now"
	body += " URGENT WIRE TRANSFER CONFIRMATION!!!"
	for i := 0; i < 5; i++ {
		body += fmt.Sprintf(" https://spam.example/offer%d", i)
	}
	return Input{
		FromAddress: fmt.Sprintf("sender%d@spam%d.example", seed, seed),
		FromName:    "Support",
		Subject:     subject,
		Body:        body,
		ContentType: "text/plain",
	}
}

// hamMessage builds a plain meeting/receipt-style message with no rule
// triggers (no money phrases, no "!", no URLs, normal case, non-spoofing
// name).
func hamMessage(seed int) Input {
	words := pickWords(hamLexicon, seed, 18)
	subject := strings.Join(words[:3], " ")
	body := strings.Join(words[3:], " ")
	body += fmt.Sprintf(" Thanks for your feedback. Best regards, the team at acme%d.example.", seed)
	return Input{
		FromAddress: fmt.Sprintf("alice%d@acme%d.example", seed, seed),
		FromName:    "Alice Smith",
		Subject:     subject,
		Body:        body,
		ContentType: "text/plain",
	}
}

func corpusScorer() *Service {
	return NewService(Config{BayesEnabled: true, MinLearns: 10, MinTokens: 11}, newMemStore())
}

func trainCorpus(t *testing.T, svc *Service) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		if err := svc.Learn(ctx, spamMessage(i), "spam"); err != nil {
			t.Fatalf("learn spam %d: %v", i, err)
		}
	}
	for i := 0; i < 50; i++ {
		if err := svc.Learn(ctx, hamMessage(i), "ham"); err != nil {
			t.Fatalf("learn ham %d: %v", i, err)
		}
	}
}

// TestBayesCorpus is the primary acceptance test: after 50/50 synthetic
// training, ten held-out spam messages must score >= 5.0 (with BAYES_SPAM)
// and ten held-out ham messages <= 0.5. Fully deterministic, no RNG.
func TestBayesCorpus(t *testing.T) {
	svc := corpusScorer()
	trainCorpus(t, svc)
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		r := svc.Score(ctx, spamMessage(1000+i))
		if r.Score < 5.0 {
			t.Errorf("held-out spam %d: score %.3f, want >= 5.0 (symbols %+v)", i, r.Score, r.Symbols)
		}
		if !hasSymbol(r.Symbols, SymbolBayesSpam) {
			t.Errorf("held-out spam %d: BAYES_SPAM missing (symbols %+v)", i, r.Symbols)
		}
	}
	for i := 0; i < 10; i++ {
		r := svc.Score(ctx, hamMessage(1000+i))
		if r.Score > 0.5 {
			t.Errorf("held-out ham %d: score %.3f, want <= 0.5 (symbols %+v)", i, r.Score, r.Symbols)
		}
	}
}

// TestBayesMinLearnsGuard: below MinLearns no BAYES symbols are emitted.
func TestBayesMinLearnsGuard(t *testing.T) {
	svc := NewService(Config{BayesEnabled: true, MinLearns: 100, MinTokens: 11}, newMemStore())
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := svc.Learn(ctx, spamMessage(i), "spam"); err != nil {
			t.Fatal(err)
		}
		if err := svc.Learn(ctx, hamMessage(i), "ham"); err != nil {
			t.Fatal(err)
		}
	}
	r := svc.Score(ctx, spamMessage(2000))
	for _, s := range r.Symbols {
		if s.Name == SymbolBayesSpam || s.Name == SymbolBayesHam {
			t.Fatalf("bayes symbol below MinLearns: %+v", s)
		}
	}
}

// TestBayesMinTokensGuard: too few kept tokens yields no BAYES symbols.
func TestBayesMinTokensGuard(t *testing.T) {
	svc := corpusScorer()
	trainCorpus(t, svc)
	r := svc.Score(context.Background(), Input{Subject: "meeting"})
	for _, s := range r.Symbols {
		if s.Name == SymbolBayesSpam || s.Name == SymbolBayesHam {
			t.Fatalf("bayes symbol below MinTokens: %+v", s)
		}
	}
}

// TestBayesNoStore: nil store is rules-only and never errors.
func TestBayesNoStore(t *testing.T) {
	svc := NewService(Config{BayesEnabled: true, MinLearns: 1, MinTokens: 1}, nil)
	r := svc.Score(context.Background(), spamMessage(1))
	if hasSymbol(r.Symbols, SymbolBayesSpam) || hasSymbol(r.Symbols, SymbolBayesHam) {
		t.Fatalf("bayes symbols with nil store: %+v", r.Symbols)
	}
	if err := svc.Learn(context.Background(), spamMessage(1), "spam"); err != nil {
		t.Fatalf("Learn with nil store: %v", err)
	}
	if err := svc.Unlearn(context.Background(), spamMessage(1), "spam"); err != nil {
		t.Fatalf("Unlearn with nil store: %v", err)
	}
}

// TestLearnUnlearnIdempotency: double-learn is a no-op; unlearn allows
// re-learn; counts floor at zero.
func TestLearnUnlearnIdempotency(t *testing.T) {
	store := newMemStore()
	svc := NewService(Config{}, store)
	ctx := context.Background()
	msg := spamMessage(1)

	if err := svc.Learn(ctx, msg, "spam"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Learn(ctx, msg, "spam"); err != nil {
		t.Fatal(err)
	}
	spam, ham, _ := store.LearnTotals(ctx)
	if spam != 1 || ham != 0 {
		t.Fatalf("totals after double learn = %v/%v, want 1/0", spam, ham)
	}

	if err := svc.Unlearn(ctx, msg, "spam"); err != nil {
		t.Fatal(err)
	}
	spam, _, _ = store.LearnTotals(ctx)
	if spam != 0 {
		t.Fatalf("totals after unlearn = %v, want 0", spam)
	}

	if err := svc.Learn(ctx, msg, "spam"); err != nil {
		t.Fatal(err)
	}
	spam, _, _ = store.LearnTotals(ctx)
	if spam != 1 {
		t.Fatalf("totals after relearn = %v, want 1", spam)
	}

	if err := svc.Learn(ctx, msg, "junk"); err == nil {
		t.Fatal("Learn accepted invalid class")
	}
}
