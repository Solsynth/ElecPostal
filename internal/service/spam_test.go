package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"gorm.io/datatypes"

	"src.solsynth.dev/sosys/elecpostal/internal/database"
	"src.solsynth.dev/sosys/elecpostal/internal/mailmime"
	"src.solsynth.dev/sosys/elecpostal/internal/spam"
)

// fakeScorer records every call and returns a canned verdict, so routing,
// threshold and learning decisions can be asserted without the real rules.
type fakeScorer struct {
	result     spam.Result
	scoreCalls int
	learned    []string
	unlearned  []string
	lastInput  spam.Input
}

func (f *fakeScorer) Score(_ context.Context, input spam.Input) spam.Result {
	f.scoreCalls++
	f.lastInput = input
	return f.result
}

func (f *fakeScorer) Learn(_ context.Context, input spam.Input, class string) error {
	f.learned = append(f.learned, class)
	f.lastInput = input
	return nil
}

func (f *fakeScorer) Unlearn(_ context.Context, input spam.Input, class string) error {
	f.unlearned = append(f.unlearned, class)
	return nil
}

func (f *fakeScorer) calls() (int, []string, []string) {
	return f.scoreCalls, f.learned, f.unlearned
}

func receivedEmail(t *testing.T, f notificationFixture, id string) database.Email {
	t.Helper()
	var stored database.Email
	if err := f.db.Where("id = ?", id).First(&stored).Error; err != nil {
		t.Fatalf("load email %s: %v", id, err)
	}
	return stored
}

func storedManifest(t *testing.T, f notificationFixture, emailID string) mailmime.Manifest {
	t.Helper()
	var source database.MessageSource
	if err := f.db.Where("email_id = ?", emailID).First(&source).Error; err != nil {
		t.Fatalf("load protocol source: %v", err)
	}
	var manifest mailmime.Manifest
	if err := json.Unmarshal(source.Manifest, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	return manifest
}

func authJSON(t *testing.T, value string) datatypes.JSON {
	t.Helper()
	if !json.Valid([]byte(value)) {
		t.Fatalf("test setup: invalid JSON %q", value)
	}
	return datatypes.JSON(value)
}

// TestReceiveEmailRoutesScoredSpam: a message whose real-rule score reaches the
// threshold lands in Spam with SpamAt set, a merged Authentication document
// and X-Spam headers in its stored protocol source.
func TestReceiveEmailRoutesScoredSpam(t *testing.T) {
	f := newNotificationFixture(t)
	f.svc.SetSpamScorer(spam.NewService(spam.Config{}, nil), 5.0, true)

	// SPF fail (1.5) + DKIM fail (1.5) + DMARC reject (2.0) = 5.0.
	authentication := authJSON(t, `{"spf":"fail","dkim":"fail","dmarc":"reject","warnings":["Possible phishing: SPF verification failed"]}`)
	email, err := f.svc.ReceiveEmail(context.Background(), ReceiveEmailInput{
		MailboxID:      f.mailbox.ID,
		FromAddress:    "sender@remote.example",
		Subject:        "Wire instructions",
		Body:           "Please review the attached invoice.",
		ContentType:    "text/plain",
		Authentication: authentication,
	})
	if err != nil {
		t.Fatalf("ReceiveEmail() error = %v", err)
	}
	if email.Folder != "spam" || email.SpamAt == nil {
		t.Fatalf("folder/spam_at = %q/%v, want spam with a timestamp", email.Folder, email.SpamAt)
	}

	var decoded struct {
		SPF      string        `json:"spf"`
		DKIM     string        `json:"dkim"`
		DMARC    string        `json:"dmarc"`
		Score    float64       `json:"score"`
		Symbols  []spam.Symbol `json:"symbols"`
		Warnings []string      `json:"warnings"`
	}
	if err := json.Unmarshal(email.Authentication, &decoded); err != nil {
		t.Fatalf("decode authentication %q: %v", email.Authentication, err)
	}
	if decoded.SPF != "fail" || decoded.DKIM != "fail" || decoded.DMARC != "reject" {
		t.Fatalf("verifier fields lost in merge: %+v", decoded)
	}
	if decoded.Score != 5.0 {
		t.Fatalf("score = %v, want 5.0", decoded.Score)
	}
	for _, want := range []string{spam.SymbolSPFFail, spam.SymbolDKIMReject, spam.SymbolDMARCPolicyReject} {
		if !hasSymbolNamed(decoded.Symbols, want) {
			t.Fatalf("symbols %+v missing %s", decoded.Symbols, want)
		}
	}
	for _, want := range []string{
		"Possible phishing: SPF verification failed",
		"Possible forgery: DKIM signature is invalid",
		"Domain policy rejects unauthenticated mail",
	} {
		if countString(decoded.Warnings, want) != 1 {
			t.Fatalf("warnings = %v, want exactly one %q", decoded.Warnings, want)
		}
	}

	manifest := storedManifest(t, f, email.ID)
	if got := headerValue(manifest.ExtraHeaders, "X-Spam-Status"); got != "Yes" {
		t.Fatalf("X-Spam-Status = %q, want Yes (headers %+v)", got, manifest.ExtraHeaders)
	}
	if got := headerValue(manifest.ExtraHeaders, "X-Spam-Score"); got != "5.00" {
		t.Fatalf("X-Spam-Score = %q, want 5.00", got)
	}
	// The stored source actually renders those headers for IMAP/POP3 clients.
	source, err := f.svc.OpenProtocolMessage(context.Background(), email.ID)
	if err != nil {
		t.Fatalf("OpenProtocolMessage() error = %v", err)
	}
	rendered, err := mailmime.RenderBytes(context.Background(), source)
	if err != nil {
		t.Fatalf("RenderBytes() error = %v", err)
	}
	if !strings.Contains(string(rendered), "X-Spam-Status: Yes\r\n") || !strings.Contains(string(rendered), "X-Spam-Score: 5.00\r\n") {
		t.Fatalf("rendered source lacks X-Spam headers:\n%s", rendered)
	}
}

// TestReceiveEmailKeepsLowScoreInInbox: below threshold means inbox, a recorded
// score, and no spam headers.
func TestReceiveEmailKeepsLowScoreInInbox(t *testing.T) {
	f := newNotificationFixture(t)
	f.svc.SetSpamScorer(spam.NewService(spam.Config{}, nil), 5.0, true)

	email, err := f.svc.ReceiveEmail(context.Background(), ReceiveEmailInput{
		MailboxID:      f.mailbox.ID,
		FromAddress:    "sender@remote.example",
		Subject:        "Meeting notes",
		Body:           "Here are the notes from today.",
		ContentType:    "text/plain",
		Authentication: authJSON(t, `{"spf":"pass"}`),
	})
	if err != nil {
		t.Fatalf("ReceiveEmail() error = %v", err)
	}
	if email.Folder != "inbox" || email.SpamAt != nil {
		t.Fatalf("folder/spam_at = %q/%v, want inbox with no spam timestamp", email.Folder, email.SpamAt)
	}
	var decoded struct {
		SPF   string  `json:"spf"`
		Score float64 `json:"score"`
	}
	if err := json.Unmarshal(email.Authentication, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.SPF != "pass" || decoded.Score != -0.2 {
		t.Fatalf("authentication = %+v, want spf pass and score -0.2", decoded)
	}
	if headers := storedManifest(t, f, email.ID).ExtraHeaders; len(headers) != 0 {
		t.Fatalf("non-spam carries headers: %+v", headers)
	}
}

// TestReceiveEmailBlockRuleScoresWithoutAuthentication: a MailBlockRule match
// alone (5.0) routes to Spam.
func TestReceiveEmailBlockRuleScoresWithoutAuthentication(t *testing.T) {
	f := newNotificationFixture(t)
	f.svc.SetSpamScorer(spam.NewService(spam.Config{}, nil), 5.0, true)
	mailboxID, workspaceID := f.mailbox.ID, f.mailbox.WorkspaceID
	if err := f.db.Create(&database.MailBlockRule{
		AccountID: f.accountID, MailboxID: &mailboxID, WorkspaceID: &workspaceID,
		Pattern: "blocked@remote.example", MatchType: "address",
	}).Error; err != nil {
		t.Fatalf("create block rule: %v", err)
	}

	email, err := f.svc.ReceiveEmail(context.Background(), ReceiveEmailInput{
		MailboxID: f.mailbox.ID, FromAddress: "blocked@remote.example",
		Subject: "Hello", Body: "Body text", ContentType: "text/plain",
	})
	if err != nil {
		t.Fatalf("ReceiveEmail() error = %v", err)
	}
	if email.Folder != "spam" {
		t.Fatalf("folder = %q, want spam for a blocked sender", email.Folder)
	}
	var decoded struct {
		Score    float64       `json:"score"`
		Symbols  []spam.Symbol `json:"symbols"`
		Warnings []string      `json:"warnings"`
	}
	if err := json.Unmarshal(email.Authentication, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Score != 5.0 || !hasSymbolNamed(decoded.Symbols, spam.SymbolBlockRule) {
		t.Fatalf("authentication = %+v, want BLOCK_RULE at 5.0", decoded)
	}
	if !containsString(decoded.Warnings, "Sender is blocked by a mailbox block rule") {
		t.Fatalf("warnings = %v, want the block-rule warning", decoded.Warnings)
	}
}

// TestReceiveEmailWithoutScorerKeepsLegacyRouting: a nil scorer leaves the
// historic phrase behavior untouched.
func TestReceiveEmailWithoutScorerKeepsLegacyRouting(t *testing.T) {
	f := newNotificationFixture(t)
	spamMail, err := f.svc.ReceiveEmail(context.Background(), ReceiveEmailInput{
		MailboxID: f.mailbox.ID, FromAddress: "sender@remote.example",
		Subject: "Offer", Body: "Cheap viagra now", ContentType: "text/plain",
	})
	if err != nil {
		t.Fatal(err)
	}
	if spamMail.Folder != "spam" || spamMail.SpamAt == nil {
		t.Fatalf("legacy phrase routing lost: folder=%q spam_at=%v", spamMail.Folder, spamMail.SpamAt)
	}
	if len(spamMail.Authentication) != 0 {
		t.Fatalf("legacy path wrote authentication: %q", spamMail.Authentication)
	}

	clean, err := f.svc.ReceiveEmail(context.Background(), ReceiveEmailInput{
		MailboxID: f.mailbox.ID, FromAddress: "sender@remote.example",
		Subject: "Meeting notes", Body: "Nothing suspicious here.", ContentType: "text/plain",
	})
	if err != nil {
		t.Fatal(err)
	}
	if clean.Folder != "inbox" {
		t.Fatalf("clean mail folder = %q, want inbox", clean.Folder)
	}
}

// TestReceiveEmailSkipsScoringForDmarcIntake: aggregate reports are never
// scored, regardless of how the intake is signalled.
func TestReceiveEmailSkipsScoringForDmarcIntake(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input ReceiveEmailInput
	}{
		{"flag", ReceiveEmailInput{DmarcIntake: true}},
		{"delivered to", ReceiveEmailInput{DeliveredTo: []string{"dmarc@example.com"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNotificationFixture(t)
			scorer := &fakeScorer{result: spam.Result{Score: 99}}
			f.svc.SetSpamScorer(scorer, 5.0, true)

			input := tc.input
			input.MailboxID = f.mailbox.ID
			input.FromAddress = "noreply@reporter.example"
			input.Subject = "Report domain: example.com"
			input.Body = "<xml>report</xml>"
			input.ContentType = "text/plain"
			email, err := f.svc.ReceiveEmail(context.Background(), input)
			if err != nil {
				t.Fatalf("ReceiveEmail() error = %v", err)
			}
			if calls, _, _ := scorer.calls(); calls != 0 {
				t.Fatalf("scorer calls = %d, want 0 for DMARC intake", calls)
			}
			if email.Folder == "spam" {
				t.Fatal("DMARC intake routed to spam")
			}
		})
	}
}

// TestReceiveEmailScoresThroughFakeScorer: the scorer receives the message
// material and its verdict drives routing.
func TestReceiveEmailScoresThroughFakeScorer(t *testing.T) {
	f := newNotificationFixture(t)
	scorer := &fakeScorer{result: spam.Result{Score: 6, Symbols: []spam.Symbol{{Name: "BAYES_SPAM", Weight: 6}}}}
	f.svc.SetSpamScorer(scorer, 5.0, true)

	email, err := f.svc.ReceiveEmail(context.Background(), ReceiveEmailInput{
		MailboxID: f.mailbox.ID, FromAddress: "sender@remote.example", FromName: "Sender",
		EnvelopeFrom: "bounce@other.example", Subject: "Subject", Body: "Body",
		ContentType: "text/plain", Authentication: authJSON(t, `{"spf":"fail"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	calls, _, _ := scorer.calls()
	if calls != 1 {
		t.Fatalf("scorer calls = %d, want 1", calls)
	}
	got := scorer.lastInput
	if got.FromAddress != "sender@remote.example" || got.EnvelopeFrom != "bounce@other.example" ||
		got.Subject != "Subject" || got.Body != "Body" || got.ContentType != "text/plain" || got.IsBlocked {
		t.Fatalf("scorer input = %+v", got)
	}
	if !strings.Contains(string(got.Authentication), `"spf":"fail"`) {
		t.Fatalf("scorer did not receive authentication: %q", got.Authentication)
	}
	if email.Folder != "spam" {
		t.Fatalf("folder = %q, want spam", email.Folder)
	}
}

// TestMoveEmailTrainsBayes: moving into Spam learns spam; moving back out
// unlearns it and learns ham; unrelated moves train nothing.
func TestMoveEmailTrainsBayes(t *testing.T) {
	f := newNotificationFixture(t)
	scorer := &fakeScorer{}
	f.svc.SetSpamScorer(scorer, 5.0, true)

	email := database.Email{
		AccountID: f.accountID, MailboxID: f.mailbox.ID, Subject: "Offer",
		Body: "cheap pills", FromAddress: "sender@remote.example", ContentType: "text/plain", Folder: "inbox",
	}
	if err := f.db.Create(&email).Error; err != nil {
		t.Fatal(err)
	}

	if err := f.svc.MoveEmail(context.Background(), f.accountID, email.ID, "spam"); err != nil {
		t.Fatal(err)
	}
	if _, learned, _ := scorer.calls(); len(learned) != 1 || learned[0] != "spam" {
		t.Fatalf("learned = %v, want [spam]", learned)
	}

	if err := f.svc.MoveEmail(context.Background(), f.accountID, email.ID, "inbox"); err != nil {
		t.Fatal(err)
	}
	_, learned, unlearned := scorer.calls()
	if len(learned) != 2 || learned[1] != "ham" {
		t.Fatalf("learned = %v, want [spam ham]", learned)
	}
	if len(unlearned) != 1 || unlearned[0] != "spam" {
		t.Fatalf("unlearned = %v, want [spam]", unlearned)
	}

	// A move that is not a spam signal trains nothing further.
	if err := f.svc.MoveEmail(context.Background(), f.accountID, email.ID, "archive"); err != nil {
		t.Fatal(err)
	}
	if _, learned, unlearned := scorer.calls(); len(learned) != 2 || len(unlearned) != 1 {
		t.Fatalf("archive move trained: learned=%v unlearned=%v", learned, unlearned)
	}

	// Moving an unknown message is still ErrNotFound.
	if err := f.svc.MoveEmail(context.Background(), f.accountID, database.NewID(), "spam"); err == nil {
		t.Fatal("MoveEmail accepted an unknown message")
	}
}

func hasSymbolNamed(symbols []spam.Symbol, name string) bool {
	for _, s := range symbols {
		if s.Name == name {
			return true
		}
	}
	return false
}

func headerValue(headers []mailmime.Header, name string) string {
	for _, h := range headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

func countString(values []string, want string) int {
	count := 0
	for _, value := range values {
		if value == want {
			count++
		}
	}
	return count
}

func containsString(values []string, want string) bool { return countString(values, want) > 0 }
