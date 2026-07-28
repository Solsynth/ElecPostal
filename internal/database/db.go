package database

import (
	"fmt"

	"src.solsynth.dev/sosys/elecpostal/internal/config"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type DB struct {
	*gorm.DB
}

func Open(cfg *config.Config) (*DB, error) {
	if cfg.Database.DSN == "" {
		return nil, fmt.Errorf("database dsn is required")
	}

	db, err := gorm.Open(postgres.Open(cfg.Database.DSN), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return nil, err
	}

	return &DB{DB: db}, nil
}

func (d *DB) AutoMigrate() error {
	if err := d.DB.AutoMigrate(
		&Mailbox{},
		&Email{},
		&Recipient{},
		&Attachment{},
		&MailProtocolCredential{},
		&EmailLabel{},
		&EmailLabelMapping{},
		&MailBlockRule{},
	); err != nil {
		return err
	}
	// Populate the quota field for messages created before RawSizeBytes was
	// introduced. Attachments are intentionally not included: DysonFS accounts
	// for their bytes independently.
	if err := d.Exec(`
		UPDATE emails
		SET raw_size_bytes = OCTET_LENGTH(subject) + OCTET_LENGTH(body) + OCTET_LENGTH(from_address) + OCTET_LENGTH(from_name) +
			COALESCE((SELECT SUM(OCTET_LENGTH(address) + OCTET_LENGTH(name) + OCTET_LENGTH(kind)) FROM recipients WHERE recipients.email_id = emails.id), 0)
		WHERE raw_size_bytes = 0
	`).Error; err != nil {
		return err
	}
	// Give pre-folder messages a useful initial home. Outbound messages have a
	// sender matching their mailbox, with or without the configured domain.
	if err := d.Exec(`
		UPDATE emails
		SET folder = CASE
			WHEN is_draft THEN 'drafts'
			WHEN EXISTS (SELECT 1 FROM mailboxes WHERE mailboxes.id = emails.mailbox_id AND (LOWER(mailboxes.address) = LOWER(emails.from_address) OR LOWER(mailboxes.address) = SPLIT_PART(LOWER(emails.from_address), '@', 1))) THEN 'sent'
			ELSE 'inbox'
		END,
		content_type = CASE WHEN content_type = '' THEN 'text/plain' ELSE content_type END
		WHERE folder = '' OR content_type = ''
	`).Error; err != nil {
		return err
	}
	if err := d.Exec(`UPDATE emails SET thread_id = id WHERE thread_id IS NULL OR thread_id = ''`).Error; err != nil {
		return err
	}
	return nil
}
