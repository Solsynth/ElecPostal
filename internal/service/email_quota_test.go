package service

import (
	"testing"

	"src.solsynth.dev/sosys/elecpostal/internal/database"
)

func TestOutgoingRawSizeExcludesDysonFSAttachments(t *testing.T) {
	email := database.Email{
		Subject:     "Hello",
		Body:        "Message body",
		FromAddress: "sender@example.com",
		FromName:    "Sender",
	}
	input := SendEmailInput{
		To:            []RecipientInput{{Address: "to@example.com", Name: "To"}},
		Cc:            []RecipientInput{{Address: "cc@example.com", Name: "CC", Kind: "cc"}},
		AttachmentIDs: []string{"dysonfs-file-1", "dysonfs-file-2"},
	}

	want := int64(len("Hello") + len("Message body") + len("sender@example.com") + len("Sender") +
		len("to@example.com") + len("To") + len("to") +
		len("cc@example.com") + len("CC") + len("cc"))
	if got := outgoingRawSize(email, input); got != want {
		t.Fatalf("outgoingRawSize() = %d, want %d", got, want)
	}
}

func TestMailboxStorageFraction(t *testing.T) {
	const planStorage = int64(10 * 1024 * 1024 * 1024)
	if got, want := planStorage/mailStorageFractionDivisor, int64(1024*1024*1024); got != want {
		t.Fatalf("mail quota = %d, want %d", got, want)
	}
}
