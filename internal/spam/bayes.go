package spam

import (
	"context"
	"math"

	"src.solsynth.dev/sosys/elecpostal/internal/logging"
)

// classify returns the Fisher-combined spam probability p in [0, 1] for
// input. 0.5 means "no evidence". Store failures log and return 0.5 — Bayes
// never blocks or fails a message.
func (s *Service) classify(ctx context.Context, input Input) float64 {
	toks := tokenizeTokens(input, s.cfg.Segmenter)
	if len(toks) == 0 {
		return 0.5
	}
	hashes := make([]uint64, len(toks))
	for i, t := range toks {
		hashes[i] = t.hash
	}

	counts, err := s.store.GetCounts(ctx, hashes)
	if err != nil {
		logging.Log.Warn().Err(err).Msg("spam: bayes get counts failed")
		return 0.5
	}
	learnsSpam, learnsHam, err := s.store.LearnTotals(ctx)
	if err != nil {
		logging.Log.Warn().Err(err).Msg("spam: bayes learn totals failed")
		return 0.5
	}
	if learnsSpam < float64(s.cfg.MinLearns) || learnsHam < float64(s.cfg.MinLearns) {
		return 0.5
	}

	kept := 0
	var spamLog, hamLog float64
	for i, tc := range counts {
		if tc.Spam <= 0 && tc.Ham <= 0 {
			continue
		}
		sf := tc.Spam / learnsSpam
		hf := tc.Ham / learnsHam
		p := sf / (sf + hf)
		n := tc.Spam + tc.Ham
		// Robinson smoothing (rspamd PROB_COMBINE with assumed 0.5).
		w := toks[i].fw * n / (1 + toks[i].fw*n)
		p = (w*0.5 + n*p) / (w + n)
		if p >= 0.4 && p <= 0.6 {
			continue // min_prob_strength 0.1
		}
		spamLog += math.Log(p)
		hamLog += math.Log(1 - p)
		kept++
	}
	if kept < s.cfg.MinTokens {
		return 0.5
	}

	// Fisher's method, ported from rspamd src/libstat/classifiers/bayes.c
	// (bayes_classify). The C code assigns
	//   h = 1 - inv_chi_square(spam_logsum, N)
	//   s = 1 - inv_chi_square(ham_logsum, N)
	// i.e. the variable named "h" is fed the spam log-sum and vice versa.
	// (The plan prose swaps these labels; the code order is what makes a
	// spammy message score high, so we follow the code.)
	hamProb := 1 - invChiSq(spamLog, kept)
	spamProb := 1 - invChiSq(hamLog, kept)
	final := (spamProb + 1 - hamProb) / 2
	if final < 0 {
		return 0
	}
	if final > 1 {
		return 1
	}
	return final
}

// invChiSq is the rspamd inv_chi_square_legacy series: the tail probability
// of a chi-square with 2*df degrees of freedom at -2*logSum. Summing exactly
// df terms matters — a fixed 100-term cap would let the series fully converge
// for every moderate log-sum and flatten every score to 0.5.
func invChiSq(logSum float64, df int) float64 {
	if logSum >= 0 {
		// No negative log-sum (e.g. no evidence): no confidence.
		return 1.0
	}
	if logSum < -700 {
		// exp(logSum) underflows: very strong confidence.
		return 0.0
	}
	m := -logSum
	term := math.Exp(logSum)
	sum := term
	for i := 1; i < df; i++ {
		term *= m / float64(i)
		sum += term
	}
	if sum > 1 {
		return 1
	}
	return sum
}
