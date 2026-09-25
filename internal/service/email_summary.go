package service

import (
	"src.solsynth.dev/sosys/elecpostal/internal/database"
	"src.solsynth.dev/sosys/elecpostal/internal/mailtext"
)

// EmailSummary is the mailbox-list representation of an email. Body contains
// readable preview text instead of the complete stored message body.
type EmailSummary struct {
	database.Email
	Body string `json:"body"`
}

func emailSummary(email database.Email) EmailSummary {
	return EmailSummary{Email: email, Body: mailtext.Summary(email.Body, email.ContentType, 256)}
}
