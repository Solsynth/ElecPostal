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

// AutoMigrate provisions the schema for this pre-release service.
func (d *DB) AutoMigrate() error {
	// Preserve the first pre-release name for installations that already used
	// mail-connections before the public API was renamed to custom domains.
	if d.Migrator().HasTable("mail_connections") && !d.Migrator().HasTable("custom_domains") {
		if err := d.Migrator().RenameTable("mail_connections", "custom_domains"); err != nil {
			return err
		}
	}
	if d.Migrator().HasTable("custom_domains") && d.Migrator().HasColumn("custom_domains", "identity") {
		if err := d.Migrator().RenameColumn("custom_domains", "identity", "domain"); err != nil {
			return err
		}
	}
	if d.Migrator().HasTable("custom_domains") && d.Migrator().HasColumn("custom_domains", "identity_type") {
		if err := d.Migrator().RenameColumn("custom_domains", "identity_type", "domain_type"); err != nil {
			return err
		}
	}
	if err := d.DB.AutoMigrate(
		&Mailbox{}, &MailboxAlias{}, &MailForwarding{}, &CustomDomain{}, &Email{}, &Recipient{}, &Attachment{},
		&MailProtocolCredential{}, &EmailLabel{}, &EmailLabelMapping{},
		&MailSendUsage{}, &MailBlockRule{}, &MessageSource{}, &MailFolder{},
		&FolderMessage{}, &MailOutbox{},
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
