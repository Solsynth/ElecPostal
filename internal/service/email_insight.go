package service

import (
	"context"

	"src.solsynth.dev/sosys/elecpostal/internal/database"
	"src.solsynth.dev/sosys/elecpostal/internal/logging"
	"src.solsynth.dev/sosys/elecpostal/internal/mailintel"
	"src.solsynth.dev/sosys/elecpostal/internal/mailtext"
	"src.solsynth.dev/sosys/elecpostal/internal/personality"
)

// mailInsight is what an incoming message revealed about itself: what a
// notification should highlight, and the summary stored on the message.
type mailInsight struct {
	// Language is the recipient's notification language.
	Language string
	// Highlight is the extracted code, security event, or action request, empty
	// when nothing stood out or the account turned highlighting off.
	Highlight mailintel.Highlight
	// Summary is the personality-service summary stored on the message.
	Summary string
}

// inspectInboxEmail analyzes a message as it is delivered, stores its summary
// when the account asked for one, and reports what the notification should say.
// Summaries are stored rather than only pushed because mailbox listings show
// them as the message preview.
//
// Analysis always runs, even when the account turned highlighting off: it is
// what keeps verification codes and security events away from the personality
// service, which only ever sees messages our own rules found nothing in.
func (s *EmailService) inspectInboxEmail(ctx context.Context, email *database.Email) mailInsight {
	language := s.accountLanguage(ctx, email.AccountID)
	preferences := s.notificationPreferences(ctx, email.AccountID)
	analysis := mailintel.Analyze(email.Subject, email.Body, email.ContentType)

	insight := mailInsight{Language: language}
	if preferences.Highlight {
		insight.Highlight = analysis
	}
	// DMARC aggregate reports are machine mail with an attached report: nobody
	// reads a summary of them, so they are never sent to the agent.
	if analysis.Kind == mailintel.KindNone && preferences.Summarize && !email.IsDmarcIntake {
		insight.Summary = s.summarizeEmail(ctx, email, language)
		email.Summary = insight.Summary
	}
	return insight
}

// summarizeEmail asks the personality service for a summary and stores it on
// the message. Every quota decision belongs to the Personality service: it
// meters these calls against the account's own usage limits and billing, so a
// refusal simply leaves the message without a summary.
func (s *EmailService) summarizeEmail(ctx context.Context, email *database.Email, language string) string {
	if s.summarizer == nil {
		return ""
	}
	summary, err := s.summarizer.Summarize(ctx, personality.SummaryRequest{
		AccountID: email.AccountID.String(),
		Language:  language,
		FromName:  email.FromName,
		Subject:   email.Subject,
		Body:      mailtext.Text(email.Body, email.ContentType),
	})
	if err != nil {
		event := logging.Log.Debug()
		message := "skipping email summary"
		if !personality.IsAccessRejection(err) {
			event = logging.Log.Warn()
			message = "failed to summarize incoming email"
		}
		event.Err(err).Str("account_id", email.AccountID.String()).Str("email_id", email.ID).Msg(message)
		return ""
	}
	if err := s.db.WithContext(ctx).Model(&database.Email{}).Where("id = ?", email.ID).Update("summary", summary).Error; err != nil {
		// The message is still worth listing and notifying with the summary we
		// have; only its stored copy is missing.
		logging.Log.Warn().Err(err).Str("email_id", email.ID).Msg("failed to store email summary")
	}
	return summary
}
