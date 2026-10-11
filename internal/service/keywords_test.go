package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"src.solsynth.dev/sosys/elecpostal/internal/database"
)

func TestSplitProtocolFlagsSeparatesTagsFromClientFlags(t *testing.T) {
	stored, keywords := SplitProtocolFlags([]string{`\Seen`, "$Junk", "Receipts", "  ", "Needs reply"})
	if strings.Join(stored, ",") != `\Seen,$Junk` {
		t.Fatalf("stored = %v, want the system flag and the scoped keyword only", stored)
	}
	if strings.Join(keywords, ",") != "Receipts,Needs reply" {
		t.Fatalf("keywords = %v, want the two tag names", keywords)
	}
}

func TestIsIMAPKeywordRejectsAtomSpecials(t *testing.T) {
	for _, name := range []string{"Receipts", "receipts-2026", "收据"} {
		if !IsIMAPKeyword(name) {
			t.Fatalf("IsIMAPKeyword(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", "Needs reply", "a]b", `a\b`, "a(b", "a*b", "a\nb", strings.Repeat("x", 256)} {
		if IsIMAPKeyword(name) {
			t.Fatalf("IsIMAPKeyword(%q) = true, want false", name)
		}
	}
}

// membershipFlagsOf reports the flags stored on a message's own membership.
func membershipFlagsOf(t *testing.T, svc *EmailService, emailID string) []string {
	t.Helper()
	var membership database.FolderMessage
	if err := svc.db.First(&membership, "email_id = ?", emailID).Error; err != nil {
		t.Fatalf("load membership: %v", err)
	}
	var flags []string
	if err := json.Unmarshal(membership.Flags, &flags); err != nil {
		t.Fatalf("decode flags: %v", err)
	}
	return flags
}

func keywordsOf(t *testing.T, svc *EmailService, emailID string) []string {
	t.Helper()
	keywords, err := svc.KeywordsForEmails(context.Background(), []string{emailID})
	if err != nil {
		t.Fatalf("KeywordsForEmails() error = %v", err)
	}
	return keywords[emailID]
}

// TestSetProtocolKeywordsCreatesAndDropsTags drives the keyword path an IMAP
// STORE or a JMAP keyword patch takes.
func TestSetProtocolKeywordsCreatesAndDropsTags(t *testing.T) {
	svc, db, accountID, mailbox, _ := newImportTestService(t)
	ctx := context.Background()
	email := createTrashTestEmail(t, db, accountID, mailbox, folderInbox, "")

	if err := svc.SetProtocolKeywords(ctx, mailbox.ID, "INBOX", []string{email.ID}, []string{"Receipts"}, "add"); err != nil {
		t.Fatalf("SetProtocolKeywords(add) error = %v", err)
	}
	// A repeated keyword resolves to the same account tag, which is what an IMAP
	// client's case-folded keyword must do.
	if err := svc.SetProtocolKeywords(ctx, mailbox.ID, "INBOX", []string{email.ID}, []string{"receipts", "Receipts"}, "add"); err != nil {
		t.Fatalf("SetProtocolKeywords(add twice) error = %v", err)
	}
	var labels []database.EmailLabel
	if err := db.Where("account_id = ?", accountID).Find(&labels).Error; err != nil {
		t.Fatalf("load tags: %v", err)
	}
	if len(labels) != 1 {
		t.Fatalf("tags = %+v, want one tag for the repeated keyword", labels)
	}
	if got := keywordsOf(t, svc, email.ID); len(got) != 1 || got[0] != "Receipts" {
		t.Fatalf("keywords = %v, want [Receipts]", got)
	}

	if err := svc.SetProtocolKeywords(ctx, mailbox.ID, "INBOX", []string{email.ID}, []string{"Receipts"}, "remove"); err != nil {
		t.Fatalf("SetProtocolKeywords(remove) error = %v", err)
	}
	if got := keywordsOf(t, svc, email.ID); len(got) != 0 {
		t.Fatalf("keywords = %v, want none after the removal", got)
	}
	// Emptying a tag must not delete it: it outlives the mail that carried it.
	var count int64
	if err := db.Model(&database.EmailLabel{}).Where("account_id = ?", accountID).Count(&count).Error; err != nil {
		t.Fatalf("count tags: %v", err)
	}
	if count != 1 {
		t.Fatalf("tags = %d, want the tag to survive its last removal", count)
	}
}

// TestReplaceKeywordsKeepsTagsTheCallerCannotObserve covers the IMAP replace
// rule: a client that cannot carry a tag name is not authoritative about it.
func TestReplaceKeywordsKeepsTagsTheCallerCannotObserve(t *testing.T) {
	svc, db, accountID, mailbox, _ := newImportTestService(t)
	ctx := context.Background()
	email := createTrashTestEmail(t, db, accountID, mailbox, folderInbox, "")

	if err := svc.SetProtocolKeywords(ctx, mailbox.ID, "INBOX", []string{email.ID}, []string{"Triaged", "Needs reply"}, "add"); err != nil {
		t.Fatalf("SetProtocolKeywords(add) error = %v", err)
	}
	if err := svc.SetProtocolKeywords(ctx, mailbox.ID, "INBOX", []string{email.ID}, []string{"Receipts"}, "replace"); err != nil {
		t.Fatalf("SetProtocolKeywords(replace) error = %v", err)
	}
	got := keywordsOf(t, svc, email.ID)
	if strings.Join(got, ",") != "Needs reply,Receipts" {
		t.Fatalf("keywords = %v, want the replaceable tag dropped and the unobservable one kept", got)
	}
	// The IMAP view carries only the name IMAP can express.
	items, _, err := svc.ListProtocolFolder(ctx, mailbox.ID, "INBOX")
	if err != nil {
		t.Fatalf("ListProtocolFolder() error = %v", err)
	}
	if len(items) != 1 || strings.Join(items[0].Keywords, ",") != "Receipts" {
		t.Fatalf("protocol keywords = %+v, want [Receipts]", items)
	}
}

// TestSetProtocolKeywordsIgnoresMessagesOutsideTheFolder covers the request
// boundary: a stale client cannot tag mail it never saw.
func TestSetProtocolKeywordsIgnoresMessagesOutsideTheFolder(t *testing.T) {
	svc, db, accountID, mailbox, _ := newImportTestService(t)
	ctx := context.Background()
	email := createTrashTestEmail(t, db, accountID, mailbox, folderInbox, "")
	ensureProtocolNamespace(t, svc, mailbox.ID)

	if err := svc.SetProtocolKeywords(ctx, mailbox.ID, "Archive", []string{email.ID}, []string{"Receipts"}, "add"); err != nil {
		t.Fatalf("SetProtocolKeywords() error = %v", err)
	}
	if got := keywordsOf(t, svc, email.ID); len(got) != 0 {
		t.Fatalf("keywords = %v, want none for a message outside the folder", got)
	}
}

// TestAppendProtocolMessageStoresKeywordsAsTags checks the APPEND split: system
// flags stay on the membership, a keyword becomes a tag.
func TestAppendProtocolMessageStoresKeywordsAsTags(t *testing.T) {
	svc, db, accountID, mailbox, _ := newImportTestService(t)
	ctx := context.Background()
	raw := []byte("From: sender@example.net\r\nTo: alice@example.com\r\nSubject: With a tag\r\n\r\nbody\r\n")

	uid, _, err := svc.AppendProtocolMessage(ctx, mailbox.ID, "INBOX", raw, []string{`\Seen`, "Receipts"}, time.Now())
	if err != nil {
		t.Fatalf("AppendProtocolMessage() error = %v", err)
	}
	var membership database.FolderMessage
	if err := db.Table("folder_messages").
		Joins("JOIN mail_folders ON mail_folders.id = folder_messages.folder_id").
		Where("mail_folders.mailbox_id = ? AND mail_folders.name = ? AND folder_messages.uid = ?", mailbox.ID, "INBOX", uid).
		First(&membership).Error; err != nil {
		t.Fatalf("load appended membership: %v", err)
	}
	flags := membershipFlagsOf(t, svc, membership.EmailID)
	if !hasSystemFlag(flags, `\Seen`) || hasSystemFlag(flags, "Receipts") {
		t.Fatalf("membership flags = %v, want \\Seen without the keyword", flags)
	}
	if got := keywordsOf(t, svc, membership.EmailID); len(got) != 1 || got[0] != "Receipts" {
		t.Fatalf("keywords = %v, want [Receipts]", got)
	}
	_ = accountID
}
