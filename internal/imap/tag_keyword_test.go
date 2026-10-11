package imap

import (
	"context"
	"strings"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"src.solsynth.dev/sosys/elecpostal/internal/config"
	"src.solsynth.dev/sosys/elecpostal/internal/database"
	"src.solsynth.dev/sosys/elecpostal/internal/service"
	"src.solsynth.dev/sosys/elecpostal/internal/workspace"
)

// stubWorkspaceProvider satisfies the workspace contract the service checks
// before it creates a mailbox, without a live provider.
type stubWorkspaceProvider struct{}

func (stubWorkspaceProvider) AuthorizeMember(context.Context, string, string) error { return nil }
func (stubWorkspaceProvider) PlanStorageBytes(context.Context, string) (int64, error) {
	return 1 << 30, nil
}
func (stubWorkspaceProvider) MailboxLimit(context.Context, string) (int64, error) { return 10, nil }
func (stubWorkspaceProvider) CustomDomainLimit(context.Context, string) (int64, error) {
	return 0, nil
}
func (stubWorkspaceProvider) SendLimits(context.Context, string) (workspace.SendLimits, error) {
	return workspace.SendLimits{}, nil
}
func (stubWorkspaceProvider) Close() error { return nil }

// TestTagKeywordRoundTripIsOneStoreEndToEnd runs the real service behind the
// real IMAP adapter: a client applies and clears a tag as a keyword, and the
// account tag store the HTTP API serves reflects both.
func TestTagKeywordRoundTripIsOneStoreEndToEnd(t *testing.T) {
	ctx := context.Background()
	db, err := gorm.Open(sqlite.Open("file:"+database.NewID()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open() error = %v", err)
	}
	if err := db.AutoMigrate(
		&database.Mailbox{}, &database.Email{}, &database.Recipient{}, &database.Attachment{},
		&database.MailProtocolCredential{}, &database.MessageSource{}, &database.MailFolder{},
		&database.FolderMessage{}, &database.EmailLabel{}, &database.EmailLabelMapping{},
	); err != nil {
		t.Fatalf("AutoMigrate() error = %v", err)
	}
	svc := service.NewEmailService(&database.DB{DB: db}, nil)
	svc.SetWorkspaceProvider(stubWorkspaceProvider{})
	accountID := uuid.New()
	mailbox, err := svc.CreateMailbox(ctx, accountID, "workspace-a", "ada@example.com", "Ada", true)
	if err != nil {
		t.Fatalf("CreateMailbox() error = %v", err)
	}
	if _, _, err := svc.AppendProtocolMessage(ctx, mailbox.ID, "INBOX",
		[]byte("From: sender@example.net\r\nTo: ada@example.com\r\nSubject: Smoke\r\n\r\nbody\r\n"), nil, time.Now()); err != nil {
		t.Fatalf("AppendProtocolMessage() error = %v", err)
	}
	credential, err := svc.CreateMailProtocolCredential(ctx, accountID, service.CreateMailProtocolCredentialInput{
		MailboxID: mailbox.ID, Label: "smoke", Protocols: []string{"imap"},
	})
	if err != nil {
		t.Fatalf("CreateMailProtocolCredential() error = %v", err)
	}

	srv, err := New(config.ListenerConfig{Enabled: true, Host: "127.0.0.1", Port: "0", TLSMode: "disabled"}, svc)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer srv.Close()

	c, err := imapclient.Dial(srv.ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer c.Logout()
	if err := c.Login("ada@example.com", credential.Secret); err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	status, err := c.Select("INBOX", false)
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if !containsFold(status.PermanentFlags, goimap.TryCreateFlag) {
		t.Fatalf("PERMANENTFLAGS = %v, want %s so a client may create a tag", status.PermanentFlags, goimap.TryCreateFlag)
	}

	seq := new(goimap.SeqSet)
	seq.AddNum(1)
	if err := c.Store(seq, goimap.AddFlags, []interface{}{"Receipts"}, nil); err != nil {
		t.Fatalf("Store(+Receipts) error = %v", err)
	}
	flags := fetchFlags(t, c, seq)
	if !containsFold(flags, "receipts") {
		t.Fatalf("FETCH FLAGS = %v, want the tag keyword", flags)
	}
	if matched, err := c.Search(&goimap.SearchCriteria{WithFlags: []string{"Receipts"}}); err != nil {
		t.Fatalf("Search(KEYWORD Receipts) error = %v", err)
	} else if len(matched) != 1 || matched[0] != 1 {
		t.Fatalf("SEARCH KEYWORD Receipts = %v, want [1]", matched)
	}
	// The keyword is the account tag, not a second vocabulary: the reader the
	// HTTP API and JMAP share sees it, and so does the tag table.
	items, _, err := svc.ListProtocolFolder(ctx, mailbox.ID, "INBOX")
	if err != nil {
		t.Fatalf("ListProtocolFolder() error = %v", err)
	}
	if len(items) != 1 || len(items[0].Keywords) != 1 || !strings.EqualFold(items[0].Keywords[0], "Receipts") {
		t.Fatalf("protocol keywords = %+v, want the tag the client stored", items)
	}
	var label database.EmailLabel
	if err := db.Where("account_id = ?", accountID).First(&label).Error; err != nil {
		t.Fatalf("load tag: %v", err)
	}
	if !strings.EqualFold(label.Name, "Receipts") {
		t.Fatalf("tag name = %q, want the keyword the client sent", label.Name)
	}

	if err := c.Store(seq, goimap.RemoveFlags, []interface{}{"Receipts"}, nil); err != nil {
		t.Fatalf("Store(-Receipts) error = %v", err)
	}
	if matched, err := c.Search(&goimap.SearchCriteria{WithFlags: []string{"Receipts"}}); err != nil {
		t.Fatalf("Search after removal error = %v", err)
	} else if len(matched) != 0 {
		t.Fatalf("SEARCH KEYWORD Receipts = %v, want none after the removal", matched)
	}
	var mappings int64
	if err := db.Model(&database.EmailLabelMapping{}).Count(&mappings).Error; err != nil {
		t.Fatalf("count mappings: %v", err)
	}
	if mappings != 0 {
		t.Fatalf("mappings = %d, want the tag off the message", mappings)
	}
	// Emptying a tag must not delete it: only the HTTP delete route does that.
	var tags int64
	if err := db.Model(&database.EmailLabel{}).Where("account_id = ?", accountID).Count(&tags).Error; err != nil {
		t.Fatalf("count tags: %v", err)
	}
	if tags != 1 {
		t.Fatalf("tags = %d, want the empty tag to survive", tags)
	}
}

func fetchFlags(t *testing.T, c *imapclient.Client, seq *goimap.SeqSet) []string {
	t.Helper()
	ch := make(chan *goimap.Message, 4)
	if err := c.Fetch(seq, []goimap.FetchItem{goimap.FetchFlags}, ch); err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	var flags []string
	for msg := range ch {
		flags = append(flags, msg.Flags...)
	}
	return flags
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}
