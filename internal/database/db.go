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
	); err != nil {
		return err
	}
	// Populate the quota field for messages created before RawSizeBytes was
	// introduced. Attachments are intentionally not included: DysonFS accounts
	// for their bytes independently.
	return d.Exec(`
		UPDATE emails
		SET raw_size_bytes = OCTET_LENGTH(subject) + OCTET_LENGTH(body) + OCTET_LENGTH(from_address) + OCTET_LENGTH(from_name) +
			COALESCE((SELECT SUM(OCTET_LENGTH(address) + OCTET_LENGTH(name) + OCTET_LENGTH(kind)) FROM recipients WHERE recipients.email_id = emails.id), 0)
		WHERE raw_size_bytes = 0
	`).Error
}
