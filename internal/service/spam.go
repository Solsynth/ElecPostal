package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gorm.io/datatypes"

	"src.solsynth.dev/sosys/elecpostal/internal/database"
	"src.solsynth.dev/sosys/elecpostal/internal/logging"
	"src.solsynth.dev/sosys/elecpostal/internal/mailmime"
	"src.solsynth.dev/sosys/elecpostal/internal/spam"
)

// Spam filter timeouts. Scoring runs inline on the delivery path, so a slow
// Bayes store must not hold the transaction open; learning happens after a
// user action and may take a little longer.
const (
	spamScoreTimeout = time.Second
	spamLearnTimeout = 5 * time.Second
)

// legacySpamPhrases are the four phrases that routed mail to Spam before the
// scorer existed. They remain the whole rule set when no scorer is configured
// and are mirrored by the scorer's MONEY_PHRASES symbol otherwise.
var legacySpamPhrases = []string{"viagra", "bitcoin giveaway", "urgent wire transfer", "click here to claim"}

// legacySpamPhraseMatch reports whether the historic phrase list matches.
func legacySpamPhraseMatch(subject, body string) bool {
	text := strings.ToLower(subject + " " + body)
	for _, phrase := range legacySpamPhrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

// spamSymbolWarnings maps the strong symbols to the warning strings documented
// on GET /api/emails. Symbols without a documented warning are omitted.
var spamSymbolWarnings = map[string]string{
	spam.SymbolSPFFail:               "Possible phishing: SPF verification failed",
	spam.SymbolDKIMReject:            "Possible forgery: DKIM signature is invalid",
	spam.SymbolDMARCPolicyReject:     "Domain policy rejects unauthenticated mail",
	spam.SymbolDMARCPolicyQuarantine: "Domain policy quarantines unauthenticated mail",
	spam.SymbolBlockRule:             "Sender is blocked by a mailbox block rule",
}

// mergeAuthentication folds the spam verdict into the SMTP-produced
// authentication document: the accumulated score and fired symbols are added,
// plus the documented warnings implied by strong symbols (deduplicated against
// the warnings the verifier already produced). The result is what the API
// exposes as Email.Authentication and what the X-Spam headers are derived
// from. When authentication cannot be decoded the verifier bytes are kept.
func mergeAuthentication(authentication datatypes.JSON, score float64, symbols []spam.Symbol) datatypes.JSON {
	merged := map[string]any{}
	if len(authentication) > 0 {
		if err := json.Unmarshal(authentication, &merged); err != nil {
			logging.Log.Warn().Err(err).Msg("decode sender authentication for merge")
			merged = map[string]any{}
		}
	}
	merged["score"] = score
	if len(symbols) > 0 {
		merged["symbols"] = symbols
	}
	warnings := stringSlice(merged["warnings"])
	for _, symbol := range symbols {
		if warning, ok := spamSymbolWarnings[symbol.Name]; ok {
			warnings = appendUniqueString(warnings, warning)
		}
	}
	if len(warnings) > 0 {
		merged["warnings"] = warnings
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		logging.Log.Warn().Err(err).Msg("encode merged authentication")
		return authentication
	}
	return encoded
}

// authenticationScore reads the recorded spam score, returning 0 when the
// document is absent or unreadable.
func authenticationScore(authentication datatypes.JSON) float64 {
	if len(authentication) == 0 {
		return 0
	}
	var decoded struct {
		Score float64 `json:"score"`
	}
	if err := json.Unmarshal(authentication, &decoded); err != nil {
		return 0
	}
	return decoded.Score
}

// trainSpamFromMove teaches the Bayes model from an explicit user move:
// moving mail into Spam trains it as spam; moving it back out untrains it and
// trains it as ham. Learning never fails the move.
func (s *EmailService) trainSpamFromMove(ctx context.Context, folder string, current database.Email) {
	if s.spamScorer == nil || strings.TrimSpace(current.ID) == "" {
		return
	}
	learnCtx, cancel := context.WithTimeout(ctx, spamLearnTimeout)
	defer cancel()
	input := spam.Input{
		FromAddress: current.FromAddress,
		Subject:     current.Subject,
		Body:        current.Body,
		ContentType: current.ContentType,
	}
	switch {
	case folder == folderSpam && current.Folder != folderSpam:
		if err := s.spamScorer.Learn(learnCtx, input, "spam"); err != nil {
			logging.Log.Warn().Err(err).Str("email_id", current.ID).Msg("spam learn failed")
		}
	case folder == folderInbox && current.Folder == folderSpam:
		if err := s.spamScorer.Unlearn(learnCtx, input, "spam"); err != nil {
			logging.Log.Warn().Err(err).Str("email_id", current.ID).Msg("spam unlearn failed")
		}
		if err := s.spamScorer.Learn(learnCtx, input, "ham"); err != nil {
			logging.Log.Warn().Err(err).Str("email_id", current.ID).Msg("spam learn failed")
		}
	}
}

// applySpamManifestHeaders adds the X-Spam headers that a stored spam message
// carries in its protocol source. It is a no-op for non-spam mail and when
// header injection is disabled.
func (s *EmailService) applySpamManifestHeaders(manifest *mailmime.Manifest, email *database.Email) {
	if manifest == nil || email == nil || !s.spamXSpamHeader || email.Folder != folderSpam {
		return
	}
	manifest.ExtraHeaders = append(manifest.ExtraHeaders,
		mailmime.Header{Name: "X-Spam-Status", Value: "Yes"},
		mailmime.Header{Name: "X-Spam-Score", Value: fmt.Sprintf("%.2f", authenticationScore(email.Authentication))},
	)
}

func stringSlice(value any) []string {
	raw, ok := value.([]any)
	if !ok {
		if list, ok := value.([]string); ok {
			return list
		}
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if text, ok := item.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

func appendUniqueString(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
