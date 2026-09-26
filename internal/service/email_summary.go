package service

import (
	"src.solsynth.dev/sosys/elecpostal/internal/database"
	"src.solsynth.dev/sosys/elecpostal/internal/mailtext"
)

// previewRuneLimit bounds the plain-text preview generated for a message that
// has no stored summary.
const previewRuneLimit = 256

// EmailSummary is the mailbox-list representation of an email. Body contains
// readable preview text instead of the complete stored message body: the
// personality-service summary when the account generated one, otherwise the
// leading text of the message. The summary stays available in Summary.
type EmailSummary struct {
	database.Email
	Body string `json:"body"`
}

func emailSummary(email database.Email) EmailSummary {
	return EmailSummary{Email: email, Body: mailtext.Preview(email.Summary, email.Body, email.ContentType, previewRuneLimit)}
}
