package jmap

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"src.solsynth.dev/sosys/elecpostal/internal/database"
	"src.solsynth.dev/sosys/elecpostal/internal/service"
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

func TestEmailObjectIncludesAttachments(t *testing.T) {
	storageKey := "att-key-1"
	thread := "thread-2"
	result := emailObject(mailRow{
		Email: database.Email{
			ID: "email-2", ThreadID: &thread, Subject: "With attachment", Body: "Body",
			FromAddress: "a@b.com", CreatedAt: time.Now(),
			Attachments: []database.Attachment{
				{ID: "att-1", Filename: "doc.pdf", MimeType: "application/pdf", Size: 1024, StorageKey: &storageKey},
			},
		},
		Folder: database.MailFolder{ID: "inbox"}, Flags: []string{},
	})
	atts := result["attachments"].([]any)
	if len(atts) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(atts))
	}
	att := atts[0].(gin.H)
	if att["name"] != "doc.pdf" || att["blobId"] != "att-key-1" || att["size"] != int64(1024) {
		t.Fatalf("attachment = %#v", att)
	}
	if result["hasAttachment"] != true {
		t.Fatal("hasAttachment should be true")
	}
}

func TestEmailObjectIncludesHeaders(t *testing.T) {
	thread := "thread-3"
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	result := emailObject(mailRow{
		Email: database.Email{
			ID: "email-3", ThreadID: &thread, Subject: "Test Headers", Body: "body",
			FromAddress: "sender@test.com", FromName: "Sender",
			CreatedAt: now, SentAt: &now,
			Recipients: []database.Recipient{{Address: "recv@test.com", Kind: "to"}},
		},
		Folder: database.MailFolder{ID: "inbox"}, Flags: []string{},
	})
	headers := result["headers"].([]any)
	if len(headers) < 3 {
		t.Fatalf("expected at least 3 headers, got %d", len(headers))
	}
	fromHeader := headers[0].(gin.H)
	if fromHeader["name"] != "From" || fromHeader["value"] != "sender@test.com" {
		t.Fatalf("from header = %#v", fromHeader)
	}
}

func TestEmailObjectIncludesTextBody(t *testing.T) {
	thread := "thread-4"
	result := emailObject(mailRow{
		Email: database.Email{
			ID: "email-4", ThreadID: &thread, Subject: "Body parts", Body: "Hello world",
			FromAddress: "a@b.com", CreatedAt: time.Now(),
			ContentType: "text/html",
		},
		Folder: database.MailFolder{ID: "inbox"}, Flags: []string{},
	})
	textBody := result["textBody"].([]any)
	if len(textBody) == 0 {
		t.Fatal("expected textBody to be non-empty")
	}
	bodyPart := textBody[0].(gin.H)
	if bodyPart["partId"] != "1" || bodyPart["blobId"] != "email-4" {
		t.Fatalf("textBody part = %#v", bodyPart)
	}
}

func TestFilterEmailsInMailbox(t *testing.T) {
	rows := []mailRow{
		{Email: database.Email{ID: "e1"}, Folder: database.MailFolder{ID: "inbox"}},
		{Email: database.Email{ID: "e2"}, Folder: database.MailFolder{ID: "sent"}},
	}
	ids := filterEmails(rows, map[string]any{"inMailbox": "inbox"})
	if len(ids) != 1 || ids[0] != "e1" {
		t.Fatalf("expected [e1], got %v", ids)
	}
}

func TestFilterEmailsInMailboxOtherThan(t *testing.T) {
	rows := []mailRow{
		{Email: database.Email{ID: "e1"}, Folder: database.MailFolder{ID: "inbox"}},
		{Email: database.Email{ID: "e2"}, Folder: database.MailFolder{ID: "sent"}},
		{Email: database.Email{ID: "e3"}, Folder: database.MailFolder{ID: "trash"}},
	}
	ids := filterEmails(rows, map[string]any{"inMailboxOtherThan": []any{"inbox"}})
	if len(ids) != 2 {
		t.Fatalf("expected 2, got %d", len(ids))
	}
}

func TestFilterEmailsByText(t *testing.T) {
	rows := []mailRow{
		{Email: database.Email{ID: "e1", Subject: "Hello World", Body: "body"}, Folder: database.MailFolder{ID: "inbox"}},
		{Email: database.Email{ID: "e2", Subject: "Goodbye", Body: "body"}, Folder: database.MailFolder{ID: "inbox"}},
	}
	ids := filterEmails(rows, map[string]any{"text": "hello"})
	if len(ids) != 1 || ids[0] != "e1" {
		t.Fatalf("expected [e1], got %v", ids)
	}
}

func TestFilterEmailsByFrom(t *testing.T) {
	rows := []mailRow{
		{Email: database.Email{ID: "e1", FromAddress: "alice@test.com"}, Folder: database.MailFolder{ID: "inbox"}},
		{Email: database.Email{ID: "e2", FromAddress: "bob@test.com"}, Folder: database.MailFolder{ID: "inbox"}},
	}
	ids := filterEmails(rows, map[string]any{"from": "alice"})
	if len(ids) != 1 || ids[0] != "e1" {
		t.Fatalf("expected [e1], got %v", ids)
	}
}

func TestFilterEmailsByTo(t *testing.T) {
	rows := []mailRow{
		{Email: database.Email{ID: "e1", Recipients: []database.Recipient{{Address: "alice@test.com", Kind: "to"}}}, Folder: database.MailFolder{ID: "inbox"}},
		{Email: database.Email{ID: "e2", Recipients: []database.Recipient{{Address: "bob@test.com", Kind: "to"}}}, Folder: database.MailFolder{ID: "inbox"}},
	}
	ids := filterEmails(rows, map[string]any{"to": "bob"})
	if len(ids) != 1 || ids[0] != "e2" {
		t.Fatalf("expected [e2], got %v", ids)
	}
}

func TestFilterEmailsBySubject(t *testing.T) {
	rows := []mailRow{
		{Email: database.Email{ID: "e1", Subject: "Meeting Tomorrow"}, Folder: database.MailFolder{ID: "inbox"}},
		{Email: database.Email{ID: "e2", Subject: "Lunch Plans"}, Folder: database.MailFolder{ID: "inbox"}},
	}
	ids := filterEmails(rows, map[string]any{"subject": "meeting"})
	if len(ids) != 1 || ids[0] != "e1" {
		t.Fatalf("expected [e1], got %v", ids)
	}
}

func TestFilterEmailsIsUnread(t *testing.T) {
	rows := []mailRow{
		{Email: database.Email{ID: "e1"}, Folder: database.MailFolder{ID: "inbox"}, Flags: []string{}},
		{Email: database.Email{ID: "e2"}, Folder: database.MailFolder{ID: "inbox"}, Flags: []string{"\\Seen"}},
	}
	ids := filterEmails(rows, map[string]any{"isUnread": true})
	if len(ids) != 1 || ids[0] != "e1" {
		t.Fatalf("expected [e1], got %v", ids)
	}
}

func TestFilterEmailsIsRead(t *testing.T) {
	rows := []mailRow{
		{Email: database.Email{ID: "e1"}, Folder: database.MailFolder{ID: "inbox"}, Flags: []string{}},
		{Email: database.Email{ID: "e2"}, Folder: database.MailFolder{ID: "inbox"}, Flags: []string{"\\Seen"}},
	}
	ids := filterEmails(rows, map[string]any{"isRead": true})
	if len(ids) != 1 || ids[0] != "e2" {
		t.Fatalf("expected [e2], got %v", ids)
	}
}

func TestFilterEmailsIsFlagged(t *testing.T) {
	rows := []mailRow{
		{Email: database.Email{ID: "e1"}, Folder: database.MailFolder{ID: "inbox"}, Flags: []string{"\\Flagged"}},
		{Email: database.Email{ID: "e2"}, Folder: database.MailFolder{ID: "inbox"}, Flags: []string{}},
	}
	ids := filterEmails(rows, map[string]any{"isFlagged": true})
	if len(ids) != 1 || ids[0] != "e1" {
		t.Fatalf("expected [e1], got %v", ids)
	}
}

func TestFilterEmailsHasAttachment(t *testing.T) {
	rows := []mailRow{
		{Email: database.Email{ID: "e1", Attachments: []database.Attachment{{ID: "a1"}}}, Folder: database.MailFolder{ID: "inbox"}},
		{Email: database.Email{ID: "e2"}, Folder: database.MailFolder{ID: "inbox"}},
	}
	ids := filterEmails(rows, map[string]any{"hasAttachment": true})
	if len(ids) != 1 || ids[0] != "e1" {
		t.Fatalf("expected [e1], got %v", ids)
	}
}

func TestFilterEmailsAfter(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	yesterday := now.Add(-24 * time.Hour)
	tomorrow := now.Add(24 * time.Hour)
	rows := []mailRow{
		{Email: database.Email{ID: "e1", CreatedAt: yesterday}, Folder: database.MailFolder{ID: "inbox"}},
		{Email: database.Email{ID: "e2", CreatedAt: now}, Folder: database.MailFolder{ID: "inbox"}},
	}
	// after=tomorrow: nothing created after tomorrow.
	ids := filterEmails(rows, map[string]any{"after": tomorrow.Format(time.RFC3339)})
	if len(ids) != 0 {
		t.Fatalf("expected 0, got %d", len(ids))
	}
	// after=yesterday+1s: only e2 (now) should pass.
	ids = filterEmails(rows, map[string]any{"after": yesterday.Add(time.Second).Format(time.RFC3339)})
	if len(ids) != 1 || ids[0] != "e2" {
		t.Fatalf("expected [e2], got %v", ids)
	}
}

func TestFilterEmailsBefore(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	yesterday := now.Add(-24 * time.Hour)
	tomorrow := now.Add(24 * time.Hour)
	rows := []mailRow{
		{Email: database.Email{ID: "e1", CreatedAt: yesterday}, Folder: database.MailFolder{ID: "inbox"}},
		{Email: database.Email{ID: "e2", CreatedAt: now}, Folder: database.MailFolder{ID: "inbox"}},
	}
	// before=tomorrow: both are before, expect both.
	ids := filterEmails(rows, map[string]any{"before": tomorrow.Format(time.RFC3339)})
	if len(ids) != 2 {
		t.Fatalf("expected 2, got %d: %v", len(ids), ids)
	}
	// before=yesterday+1s: only e1 (yesterday) should pass.
	ids = filterEmails(rows, map[string]any{"before": yesterday.Add(time.Second).Format(time.RFC3339)})
	if len(ids) != 1 || ids[0] != "e1" {
		t.Fatalf("expected [e1], got %v", ids)
	}
}

func TestFilterEmailsNilFilterReturnsAll(t *testing.T) {
	rows := []mailRow{
		{Email: database.Email{ID: "e1"}, Folder: database.MailFolder{ID: "inbox"}},
		{Email: database.Email{ID: "e2"}, Folder: database.MailFolder{ID: "inbox"}},
	}
	ids := filterEmails(rows, nil)
	if len(ids) != 2 {
		t.Fatalf("expected 2, got %d", len(ids))
	}
}

func TestSortEmailsBySubject(t *testing.T) {
	rows := []mailRow{
		{Email: database.Email{ID: "e1", Subject: "Zebra"}, Folder: database.MailFolder{ID: "inbox"}},
		{Email: database.Email{ID: "e2", Subject: "Apple"}, Folder: database.MailFolder{ID: "inbox"}},
		{Email: database.Email{ID: "e3", Subject: "Mango"}, Folder: database.MailFolder{ID: "inbox"}},
	}
	ids := []string{"e1", "e2", "e3"}
	sortEmails(rows, ids, "subject", true)
	if ids[0] != "e2" || ids[1] != "e3" || ids[2] != "e1" {
		t.Fatalf("unexpected sort order: %v", ids)
	}
}

func TestSortEmailsBySize(t *testing.T) {
	rows := []mailRow{
		{Email: database.Email{ID: "e1"}, Folder: database.MailFolder{ID: "inbox"}, WireSize: 100},
		{Email: database.Email{ID: "e2"}, Folder: database.MailFolder{ID: "inbox"}, WireSize: 500},
		{Email: database.Email{ID: "e3"}, Folder: database.MailFolder{ID: "inbox"}, WireSize: 200},
	}
	ids := []string{"e1", "e2", "e3"}
	sortEmails(rows, ids, "size", true)
	if ids[0] != "e1" || ids[1] != "e3" || ids[2] != "e2" {
		t.Fatalf("unexpected sort order: %v", ids)
	}
}

func TestSortEmailsByFrom(t *testing.T) {
	rows := []mailRow{
		{Email: database.Email{ID: "e1", FromAddress: "charlie@test.com"}, Folder: database.MailFolder{ID: "inbox"}},
		{Email: database.Email{ID: "e2", FromAddress: "alice@test.com"}, Folder: database.MailFolder{ID: "inbox"}},
	}
	ids := []string{"e1", "e2"}
	sortEmails(rows, ids, "from", true)
	if ids[0] != "e2" || ids[1] != "e1" {
		t.Fatalf("unexpected sort order: %v", ids)
	}
}

func TestSortEmailsDescending(t *testing.T) {
	now := time.Now()
	yesterday := now.Add(-24 * time.Hour)
	rows := []mailRow{
		{Email: database.Email{ID: "e1", CreatedAt: yesterday}, Folder: database.MailFolder{ID: "inbox"}},
		{Email: database.Email{ID: "e2", CreatedAt: now}, Folder: database.MailFolder{ID: "inbox"}},
	}
	ids := []string{"e1", "e2"}
	sortEmails(rows, ids, "receivedAt", false)
	if ids[0] != "e2" || ids[1] != "e1" {
		t.Fatalf("unexpected sort order: %v", ids)
	}
}

func TestExtractSnippetWithQuery(t *testing.T) {
	snippet := extractSnippet("The quick brown fox jumps over the lazy dog", "fox", 50)
	if len(snippet) == 0 {
		t.Fatal("expected non-empty snippet")
	}
	if !strings.Contains(snippet, "<mark>fox</mark>") {
		t.Fatalf("snippet should contain highlighted term, got: %s", snippet)
	}
}

func TestExtractSnippetNoQuery(t *testing.T) {
	snippet := extractSnippet("Short text", "", 50)
	if snippet != "Short text" {
		t.Fatalf("expected 'Short text', got: %s", snippet)
	}
}

func TestExtractSnippetLongText(t *testing.T) {
	longText := strings.Repeat("a", 1000)
	snippet := extractSnippet(longText, "", 100)
	if len(snippet) > 100 {
		t.Fatalf("expected snippet <= 100 chars, got %d", len(snippet))
	}
}

func TestExtractSnippetTruncation(t *testing.T) {
	text := "beginning middle end of a very long text that goes on and on and on"
	snippet := extractSnippet(text, "middle", 20)
	if len(snippet) >= len(text) {
		t.Fatalf("expected truncation, got full text: %s", snippet)
	}
}

func TestExtractSnippetQueryNotFound(t *testing.T) {
	snippet := extractSnippet("Hello world", "xyz", 100)
	if snippet != "Hello world" {
		t.Fatalf("expected 'Hello world', got: %s", snippet)
	}
}

func TestThreadIDWithExplicitThread(t *testing.T) {
	thread := "thread-abc"
	e := database.Email{ID: "email-1", ThreadID: &thread}
	if tid := threadID(e); tid != "thread-abc" {
		t.Fatalf("expected thread-abc, got %s", tid)
	}
}

func TestThreadIDFallbackToEmailID(t *testing.T) {
	e := database.Email{ID: "email-xyz"}
	if tid := threadID(e); tid != "email-xyz" {
		t.Fatalf("expected email-xyz, got %s", tid)
	}
}

func TestSetFlagAddAndRemove(t *testing.T) {
	flags := []string{"\\Seen"}
	flags = setFlag(flags, "\\Flagged", true)
	if len(flags) != 2 {
		t.Fatalf("expected 2 flags, got %d: %v", len(flags), flags)
	}
	flags = setFlag(flags, "\\Flagged", false)
	if len(flags) != 1 || flags[0] != "\\Seen" {
		t.Fatalf("expected [\\Seen], got %v", flags)
	}
}

func TestSetFlagNoDuplicates(t *testing.T) {
	flags := []string{"\\Seen"}
	flags = setFlag(flags, "\\Seen", true)
	if len(flags) != 1 {
		t.Fatalf("expected 1 flag (no duplicate), got %d: %v", len(flags), flags)
	}
}

func TestClamp(t *testing.T) {
	if Clamp(5, 0, 10) != 5 {
		t.Fatal("clamp middle failed")
	}
	if Clamp(-1, 0, 10) != 0 {
		t.Fatal("clamp low failed")
	}
	if Clamp(15, 0, 10) != 10 {
		t.Fatal("clamp high failed")
	}
}

func TestIdSet(t *testing.T) {
	s := idSet([]any{"a", "b", "c"})
	if !s["a"] || !s["b"] || !s["c"] || s["d"] {
		t.Fatalf("unexpected idSet: %v", s)
	}
}

func TestStringList(t *testing.T) {
	list := stringList([]any{"x", "y", 123, "z"})
	if len(list) != 3 || list[0] != "x" || list[2] != "z" {
		t.Fatalf("unexpected stringList: %v", list)
	}
}

func TestWellKnownEndpoint(t *testing.T) {
	_ = WellKnown
}

func TestFolderObjectAllRoles(t *testing.T) {
	roles := map[string]string{
		`\Inbox`: "inbox", `\Sent`: "sent", `\Drafts`: "drafts",
		`\Junk`: "junk", `\Trash`: "trash", `\Archive`: "archive", "": "",
	}
	for use, expected := range roles {
		result := folderObject(database.MailFolder{SpecialUse: use})
		if result["role"] != expected {
			t.Fatalf("specialUse %q -> role %q, expected %q", use, result["role"], expected)
		}
	}
}

func TestMailboxPatch(t *testing.T) {
	patch := map[string]any{"mailboxIds/abc123": true, "keywords/$seen": true}
	id := mailboxPatch(patch)
	if id != "abc123" {
		t.Fatalf("expected abc123, got %s", id)
	}
	id = mailboxPatch(map[string]any{"name": "New Name"})
	if id != "" {
		t.Fatalf("expected empty, got %s", id)
	}
}

func TestEmailObjectPreviewUsesTheStoredSummary(t *testing.T) {
	thread := "thread-preview"
	result := emailObject(mailRow{
		Email: database.Email{
			ID: "email-preview", ThreadID: &thread, Subject: "This week at Acme",
			Body:        `<html><body><p>Hello Ada, here is everything that shipped this week.</p></body></html>`,
			ContentType: "text/html",
			Summary:     "Acme shipped three features this week",
			FromAddress: "hello@acme.example", CreatedAt: time.Now(),
		},
		Folder: database.MailFolder{ID: "inbox"}, Flags: []string{},
	})
	if got := result["preview"]; got != "Acme shipped three features this week" {
		t.Fatalf("preview = %v, want the stored summary", got)
	}
}

// ── Tag keywords ─────────────────────────────────────────────────────────

func TestEmailObjectIncludesTagKeywords(t *testing.T) {
	thread := "thread-tagged"
	result := emailObject(mailRow{
		Email: database.Email{
			ID: "email-tagged", ThreadID: &thread, Subject: "Tagged", Body: "Body",
			FromAddress: "sender@example.test", CreatedAt: time.Now(),
		},
		Folder:   database.MailFolder{ID: "inbox"},
		Flags:    []string{"\\Seen"},
		Keywords: []string{"todo", "needs reply"},
	})
	keywords := result["keywords"].(gin.H)
	if keywords["$seen"] != true {
		t.Fatalf("system keyword lost: %#v", keywords)
	}
	// JMAP carries any keyword name, spaces included.
	if keywords["todo"] != true || keywords["needs reply"] != true {
		t.Fatalf("tag keywords missing: %#v", keywords)
	}
}

func TestFilterEmailsHasKeyword(t *testing.T) {
	rows := []mailRow{
		{Email: database.Email{ID: "e1"}, Folder: database.MailFolder{ID: "inbox"}, Keywords: []string{"todo"}},
		{Email: database.Email{ID: "e2"}, Folder: database.MailFolder{ID: "inbox"}, Keywords: []string{"Receipts"}},
		{Email: database.Email{ID: "e3"}, Folder: database.MailFolder{ID: "inbox"}},
	}
	// Keywords fold case, so an uppercased request still matches the stored tag.
	ids := filterEmails(rows, map[string]any{"hasKeyword": "TODO"})
	if len(ids) != 1 || ids[0] != "e1" {
		t.Fatalf("expected [e1], got %v", ids)
	}
}

func TestFilterEmailsHasKeywordSystemFlag(t *testing.T) {
	rows := []mailRow{
		{Email: database.Email{ID: "e1"}, Folder: database.MailFolder{ID: "inbox"}, Flags: []string{"\\Seen"}},
		{Email: database.Email{ID: "e2"}, Folder: database.MailFolder{ID: "inbox"}, Flags: []string{}},
	}
	ids := filterEmails(rows, map[string]any{"hasKeyword": "$seen"})
	if len(ids) != 1 || ids[0] != "e1" {
		t.Fatalf("expected [e1], got %v", ids)
	}
}

func TestFilterEmailsNotKeyword(t *testing.T) {
	rows := []mailRow{
		{Email: database.Email{ID: "e1"}, Folder: database.MailFolder{ID: "inbox"}, Keywords: []string{"todo"}},
		{Email: database.Email{ID: "e2"}, Folder: database.MailFolder{ID: "inbox"}, Keywords: []string{"Receipts"}},
	}
	ids := filterEmails(rows, map[string]any{"notKeyword": "todo"})
	if len(ids) != 1 || ids[0] != "e2" {
		t.Fatalf("expected [e2], got %v", ids)
	}
}

// newKeywordTestHandler builds an in-memory store holding one mailbox with an
// INBOX: the smallest fixture the keyword wiring reaches through.
func newKeywordTestHandler(t *testing.T) (*Handler, *gorm.DB, database.Mailbox, database.MailFolder) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+database.NewID()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open() error = %v", err)
	}
	if err := db.AutoMigrate(
		&database.Mailbox{}, &database.Email{}, &database.Recipient{}, &database.Attachment{},
		&database.MessageSource{}, &database.MailFolder{}, &database.FolderMessage{},
		&database.EmailLabel{}, &database.EmailLabelMapping{},
	); err != nil {
		t.Fatalf("AutoMigrate() error = %v", err)
	}
	accountID := uuid.New()
	mailbox := database.Mailbox{ID: database.NewID(), AccountID: accountID, WorkspaceID: "workspace-a", Address: "ada@example.com", Name: "Ada"}
	if err := db.Create(&mailbox).Error; err != nil {
		t.Fatalf("create mailbox: %v", err)
	}
	folder := database.MailFolder{MailboxID: mailbox.ID, Name: "INBOX", SpecialUse: `\Inbox`, NextUID: 1, HighestModSeq: 1}
	if err := db.Create(&folder).Error; err != nil {
		t.Fatalf("create folder: %v", err)
	}
	return New(service.NewEmailService(&database.DB{DB: db}, nil)), db, mailbox, folder
}

// seedKeywordEmail stores one message, its protocol source, and its INBOX
// membership, which is what rows() reads.
func seedKeywordEmail(t *testing.T, db *gorm.DB, mailbox database.Mailbox, folder database.MailFolder, subject string) database.Email {
	t.Helper()
	email := database.Email{
		ID: database.NewID(), AccountID: mailbox.AccountID, MailboxID: mailbox.ID,
		Subject: subject, Body: "Body", Folder: folder.Name,
		FromAddress: "sender@example.test",
	}
	if err := db.Create(&email).Error; err != nil {
		t.Fatalf("create email: %v", err)
	}
	source := database.MessageSource{EmailID: email.ID, WireSizeBytes: 128, ReceivedAt: email.CreatedAt}
	if err := db.Create(&source).Error; err != nil {
		t.Fatalf("create message source: %v", err)
	}
	var members int64
	if err := db.Model(&database.FolderMessage{}).Where("folder_id = ?", folder.ID).Count(&members).Error; err != nil {
		t.Fatalf("count memberships: %v", err)
	}
	membership := database.FolderMessage{
		FolderID: folder.ID, EmailID: email.ID, UID: uint32(members) + 1,
		Flags: datatypes.JSON([]byte("[]")), ModSeq: folder.HighestModSeq,
	}
	if err := db.Create(&membership).Error; err != nil {
		t.Fatalf("create membership: %v", err)
	}
	return email
}

func keywordTags(t *testing.T, h *Handler, emailID string) []string {
	t.Helper()
	byEmail, err := h.mail.KeywordsForEmails(context.Background(), []string{emailID})
	if err != nil {
		t.Fatalf("KeywordsForEmails() error = %v", err)
	}
	return byEmail[emailID]
}

func membershipFlagsOf(t *testing.T, db *gorm.DB, folderID, emailID string) []string {
	t.Helper()
	var membership database.FolderMessage
	if err := db.Where("folder_id = ? AND email_id = ?", folderID, emailID).First(&membership).Error; err != nil {
		t.Fatalf("load membership: %v", err)
	}
	var flags []string
	if err := json.Unmarshal(membership.Flags, &flags); err != nil {
		t.Fatalf("decode flags: %v", err)
	}
	return flags
}

func newKeywordTestContext() *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/jmap/api", nil)
	return c
}

func TestEmailSetKeywordPatchAddsAndRemovesTag(t *testing.T) {
	h, db, mailbox, folder := newKeywordTestHandler(t)
	email := seedKeywordEmail(t, db, mailbox, folder, "Tagged")
	// An unrelated tag proves the patch adds to the message's tag set instead
	// of replacing it.
	if err := h.mail.SetProtocolKeywords(context.Background(), mailbox.ID, folder.Name, []string{email.ID}, []string{"keep"}, "add"); err != nil {
		t.Fatalf("SetProtocolKeywords() error = %v", err)
	}
	c := newKeywordTestContext()

	result := h.emailSet(c, uuid.Nil, mailbox.ID, map[string]any{
		"update": map[string]any{email.ID: map[string]any{"keywords/todo": true}},
	})
	if _, ok := result["updated"].(gin.H)[email.ID]; !ok {
		t.Fatalf("keyword add not applied: %#v", result)
	}
	if tags := keywordTags(t, h, email.ID); strings.Join(tags, ",") != "keep,todo" {
		t.Fatalf("tags after add = %v, want [keep todo]", tags)
	}

	result = h.emailSet(c, uuid.Nil, mailbox.ID, map[string]any{
		"update": map[string]any{email.ID: map[string]any{"keywords/todo": false}},
	})
	if _, ok := result["updated"].(gin.H)[email.ID]; !ok {
		t.Fatalf("keyword remove not applied: %#v", result)
	}
	if tags := keywordTags(t, h, email.ID); strings.Join(tags, ",") != "keep" {
		t.Fatalf("tags after remove = %v, want [keep]", tags)
	}
}

func TestEmailQueryHasKeywordReturnsTaggedMail(t *testing.T) {
	h, db, mailbox, folder := newKeywordTestHandler(t)
	tagged := seedKeywordEmail(t, db, mailbox, folder, "Tagged")
	seedKeywordEmail(t, db, mailbox, folder, "Untagged")
	if err := h.mail.SetProtocolKeywords(context.Background(), mailbox.ID, folder.Name, []string{tagged.ID}, []string{"todo"}, "add"); err != nil {
		t.Fatalf("SetProtocolKeywords() error = %v", err)
	}
	c := newKeywordTestContext()

	result := h.emailQuery(c, mailbox.ID, map[string]any{"filter": map[string]any{"hasKeyword": "todo"}})
	ids, _ := result["ids"].([]string)
	if len(ids) != 1 || ids[0] != tagged.ID {
		t.Fatalf("hasKeyword ids = %v, want [%s]", ids, tagged.ID)
	}
}

func TestEmailImportRoutesTagKeywordsAndSystemFlags(t *testing.T) {
	h, db, mailbox, folder := newKeywordTestHandler(t)
	email := seedKeywordEmail(t, db, mailbox, folder, "Imported")
	c := newKeywordTestContext()

	result := h.emailImport(c, mailbox.ID, map[string]any{
		"emails": []any{map[string]any{
			"blobId":   email.ID,
			"keywords": map[string]any{"$seen": true, "$Junk": true, "todo": true},
		}},
	})
	if _, ok := result["created"].(gin.H)[email.ID]; !ok {
		t.Fatalf("import not applied: %#v", result)
	}
	flags := membershipFlagsOf(t, db, folder.ID, email.ID)
	if strings.Join(flags, ",") != "$Junk,\\Seen" {
		t.Fatalf("flags = %v, want the system flag and the scoped keyword only", flags)
	}
	// A keyword that became a flag must not also become a tag.
	if tags := keywordTags(t, h, email.ID); len(tags) != 1 || tags[0] != "todo" {
		t.Fatalf("tags after import = %v, want [todo]", tags)
	}
}
