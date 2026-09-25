package service

import (
	"context"
	"testing"

	"src.solsynth.dev/sosys/elecpostal/internal/database"
)

func TestListEmailsSummarizesHTMLWithoutMutatingStoredBody(t *testing.T) {
	svc, db, accountID, mailbox, _ := newImportTestService(t)
	body := `<html><head><style>.x { color: red }</style></head><body><p>Hello <b>mail</b></p><script>alert(1)</script></body></html>`
	email := database.Email{
		ID: database.NewID(), AccountID: accountID, MailboxID: mailbox.ID, Subject: "Preview",
		Body: body, ContentType: "text/html", FromAddress: "sender@example.test",
	}
	if err := db.Create(&email).Error; err != nil {
		t.Fatal(err)
	}
	items, _, err := svc.ListEmails(context.Background(), accountID, mailbox.ID, ListInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Body != "Hello mail" {
		t.Fatalf("list summary = %#v, want body %q", items, "Hello mail")
	}
	var stored database.Email
	if err := db.First(&stored, "id = ?", email.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Body != body {
		t.Fatalf("stored body changed: %q", stored.Body)
	}
	full, err := svc.GetEmail(context.Background(), accountID, email.ID)
	if err != nil {
		t.Fatal(err)
	}
	if full.Body != body {
		t.Fatalf("detail body = %q, want original HTML", full.Body)
	}
}
