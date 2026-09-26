package service

import (
	"context"

	"github.com/google/uuid"

	"src.solsynth.dev/sosys/elecpostal/internal/account"
	"src.solsynth.dev/sosys/elecpostal/internal/database"
	"src.solsynth.dev/sosys/elecpostal/internal/logging"
	"src.solsynth.dev/sosys/elecpostal/internal/mailintel"
	"src.solsynth.dev/sosys/elecpostal/internal/mailtext"
	"src.solsynth.dev/sosys/elecpostal/internal/personality"
	"src.solsynth.dev/sosys/elecpostal/internal/ring"
)

// UpdateNotificationSettingsInput carries the notification preference fields to
// change. Omitted fields keep their stored value.
type UpdateNotificationSettingsInput struct {
	Highlight *bool `json:"highlight"`
	Summarize *bool `json:"summarize"`
}

// SetAccountLanguageProvider enables localized notifications.
func (s *EmailService) SetAccountLanguageProvider(provider account.Provider) {
	s.language = provider
}

// SetNotificationSummarizer enables personality-service summaries.
func (s *EmailService) SetNotificationSummarizer(summarizer personality.Summarizer) {
	s.summarizer = summarizer
}

// NotificationSettings returns the account's notification preferences, or the
// defaults when the account never changed them.
func (s *EmailService) NotificationSettings(ctx context.Context, accountID uuid.UUID) (database.AccountNotificationSettings, error) {
	var settings database.AccountNotificationSettings
	// Find rather than First: an account that never changed anything is the
	// common case on the delivery path and must not log a not-found error.
	if err := s.db.WithContext(ctx).Where("account_id = ?", accountID).Limit(1).Find(&settings).Error; err != nil {
		return database.AccountNotificationSettings{}, err
	}
	if settings.AccountID == uuid.Nil {
		return database.DefaultNotificationSettings(accountID), nil
	}
	return settings, nil
}

// UpdateNotificationSettings stores the fields the caller provided and returns
// the resulting preferences.
func (s *EmailService) UpdateNotificationSettings(ctx context.Context, accountID uuid.UUID, input UpdateNotificationSettingsInput) (database.AccountNotificationSettings, error) {
	settings, err := s.NotificationSettings(ctx, accountID)
	if err != nil {
		return database.AccountNotificationSettings{}, err
	}
	if input.Highlight != nil {
		settings.Highlight = *input.Highlight
	}
	if input.Summarize != nil {
		settings.Summarize = *input.Summarize
	}
	if settings.CreatedAt.IsZero() {
		if err := s.db.WithContext(ctx).Create(&settings).Error; err != nil {
			return database.AccountNotificationSettings{}, err
		}
	} else if err := s.db.WithContext(ctx).Save(&settings).Error; err != nil {
		return database.AccountNotificationSettings{}, err
	}
	return settings, nil
}

// notificationPreferences returns stored preferences, falling back to the
// defaults. A lookup failure keeps notifications working with the defaults
// rather than dropping them.
func (s *EmailService) notificationPreferences(ctx context.Context, accountID uuid.UUID) database.AccountNotificationSettings {
	settings, err := s.NotificationSettings(ctx, accountID)
	if err != nil {
		logging.Log.Warn().Err(err).Str("account_id", accountID.String()).Msg("failed to load notification settings")
		return database.DefaultNotificationSettings(accountID)
	}
	return settings
}

// buildEmailNotification decides what a new message's notification says.
//
// Message analysis always runs, even when the account turned highlighting off:
// it is what keeps verification codes and security events away from the
// personality service, which only ever sees messages our own rules found
// nothing in.
func (s *EmailService) buildEmailNotification(ctx context.Context, email *database.Email) ring.EmailNotification {
	language := s.notificationLanguage(ctx, email.AccountID)
	preferences := s.notificationPreferences(ctx, email.AccountID)
	analysis := mailintel.Analyze(email.Subject, email.Body, email.ContentType)

	notification := ring.EmailNotification{
		AccountID: email.AccountID.String(),
		EmailID:   email.ID,
		Language:  language,
		Subject:   email.Subject,
		FromName:  email.FromName,
	}
	if preferences.Highlight {
		notification.Highlight = analysis
	}
	if analysis.Kind == mailintel.KindNone && preferences.Summarize {
		notification.Summary = s.summarizeForNotification(ctx, email, language)
	}
	return notification
}

// notificationLanguage resolves the recipient's language, degrading to the
// default locale when the account service cannot answer.
func (s *EmailService) notificationLanguage(ctx context.Context, accountID uuid.UUID) string {
	if s.language == nil {
		return ""
	}
	language, err := s.language.Language(ctx, accountID.String())
	if err != nil {
		logging.Log.Warn().Err(err).Str("account_id", accountID.String()).Msg("failed to resolve notification language")
		return ""
	}
	return language
}

// summarizeForNotification asks the personality service for a summary and
// stores it on the message. Every quota decision belongs to the Personality
// service: it meters these calls against the account's own usage limits and
// billing, so a refusal simply leaves the notification on its subject fallback.
func (s *EmailService) summarizeForNotification(ctx context.Context, email *database.Email, language string) string {
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
		message := "skipping notification summary"
		if !personality.IsAccessRejection(err) {
			event = logging.Log.Warn()
			message = "failed to summarize incoming email"
		}
		event.Err(err).Str("account_id", email.AccountID.String()).Str("email_id", email.ID).Msg(message)
		return ""
	}
	if err := s.db.WithContext(ctx).Model(&database.Email{}).Where("id = ?", email.ID).Update("summary", summary).Error; err != nil {
		// The notification is still worth sending with the summary we have.
		logging.Log.Warn().Err(err).Str("email_id", email.ID).Msg("failed to store notification summary")
	}
	return summary
}
