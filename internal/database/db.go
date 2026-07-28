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
	return d.DB.AutoMigrate(
		&Mailbox{},
		&Email{},
		&Recipient{},
		&Attachment{},
		&MailProtocolCredential{},
		&EmailLabel{},
		&EmailLabelMapping{},
	)
}
