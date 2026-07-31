package jmap

import (
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"src.solsynth.dev/sosys/elecpostal/internal/database"
)

func TestEmailObjectMapsProtocolState(t *testing.T) {
	thread := "thread-1"
	result := emailObject(mailRow{
		Email: database.Email{
			ID: "email-1", ThreadID: &thread, Subject: "Hello", Body: "A message body",
			FromAddress: "sender@example.test", CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
			Recipients: []database.Recipient{{Address: "recipient@example.test", Kind: "to"}},
		},
		Folder: database.MailFolder{ID: "inbox"}, Flags: []string{"\\Seen", "\\Flagged"},
	})
	keywords := result["keywords"].(gin.H)
	if keywords["$seen"] != true || keywords["$flagged"] != true {
		t.Fatalf("keywords = %#v", keywords)
	}
	if result["threadId"] != thread || result["mailboxIds"].(gin.H)["inbox"] != true {
		t.Fatalf("email object = %#v", result)
	}
}

func TestFolderObjectMapsSpecialUseRole(t *testing.T) {
	result := folderObject(database.MailFolder{ID: "sent", Name: "Sent", SpecialUse: `\Sent`, Subscribed: true})
	if result["role"] != "sent" || result["isSubscribed"] != true {
		t.Fatalf("mailbox object = %#v", result)
	}
}
