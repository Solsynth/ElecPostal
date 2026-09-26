package service

import (
	"context"

	"github.com/google/uuid"

	"src.solsynth.dev/sosys/elecpostal/internal/account"
	"src.solsynth.dev/sosys/elecpostal/internal/database"
	"src.solsynth.dev/sosys/elecpostal/internal/logging"
	"src.solsynth.dev/sosys/elecpostal/internal/personality"
	"src.solsynth.dev/sosys/elecpostal/internal/ring"
)

// UpdateNotificationSettingsInput carries the notification preference fields to
// change. Omitted fields keep their stored value.
type UpdateNotificationSettingsInput struct {
	Highlight *bool `json:"highlight"`
	Summarize *bool `json:"summarize"`
}

// SetAccountLanguageProvider enables localized notifications and summaries.
func (s *EmailService) SetAccountLanguageProvider(provider account.Provider) {
	s.language = provider
}

// SetSummarizer enables personality-service summaries. They back both the
// notification subtitle and the stored message preview.
func (s *EmailService) SetSummarizer(summarizer personality.Summarizer) {
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

// accountLanguage resolves the recipient's language, degrading to the default
// locale when the account service cannot answer.
func (s *EmailService) accountLanguage(ctx context.Context, accountID uuid.UUID) string {
	if s.language == nil {
		return ""
	}
	language, err := s.language.Language(ctx, accountID.String())
	if err != nil {
		logging.Log.Warn().Err(err).Str("account_id", accountID.String()).Msg("failed to resolve account language")
		return ""
	}
	return language
}

// emailNotificationPayload maps a delivered message and its insight onto the
// push Ring delivers.
func emailNotificationPayload(email *database.Email, insight mailInsight) ring.EmailNotification {
	return ring.EmailNotification{
		AccountID: email.AccountID.String(),
		EmailID:   email.ID,
		Language:  insight.Language,
		Subject:   email.Subject,
		FromName:  email.FromName,
		Highlight: insight.Highlight,
		Summary:   insight.Summary,
	}
}
