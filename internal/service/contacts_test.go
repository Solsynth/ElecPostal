package service

import (
	"context"
	"net"
	"strings"
	"testing"

	"src.solsynth.dev/sosys/elecpostal/internal/database"
)

func TestAddressHistoryAutocompleteAndAliasIdentity(t *testing.T) {
	svc, db, accountID, mailbox, _ := newImportTestService(t)
	alias := database.MailboxAlias{
		ID: database.NewID(), MailboxID: mailbox.ID, Address: "support@example.com", Name: "Support",
	}
	if err := db.Create(&alias).Error; err != nil {
		t.Fatal(err)
	}
	inbound := database.Email{
		ID: database.NewID(), AccountID: accountID, MailboxID: mailbox.ID,
		FromAddress: "support@example.com", Folder: "inbox",
	}
	outbound := database.Email{
		ID: database.NewID(), AccountID: accountID, MailboxID: mailbox.ID,
		FromAddress: mailbox.Address, Folder: "sent",
	}
	for _, email := range []database.Email{inbound, outbound} {
		if err := db.Create(&email).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&database.Recipient{
		ID: database.NewID(), EmailID: outbound.ID, Address: "person@example.net", Name: "Person", Kind: "to",
	}).Error; err != nil {
		t.Fatal(err)
	}

	senders, err := svc.ListSenders(context.Background(), accountID, "SUPPORT", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(senders) != 1 || senders[0].Address != alias.Address || senders[0].Name != alias.Name || !senders[0].Alias || senders[0].WorkspaceID != mailbox.WorkspaceID {
		t.Fatalf("sender suggestion = %#v", senders)
	}
	if !strings.Contains(senders[0].GravatarURL, "?d=identicon&s=80") || senders[0].AvatarSource != "gravatar" {
		t.Fatalf("sender avatar = %#v", senders[0])
	}

	contacts, err := svc.ListContacts(context.Background(), accountID, "PERSON", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(contacts) != 1 || contacts[0].Address != "person@example.net" || contacts[0].Name != "Person" {
		t.Fatalf("contact suggestion = %#v", contacts)
	}

}

type testBIMIDNS struct {
	records map[string][]string
}

func (dns testBIMIDNS) LookupCNAME(context.Context, string) (string, error) { return "", nil }
func (dns testBIMIDNS) LookupTXT(_ context.Context, name string) ([]string, error) {
	return dns.records[name], nil
}
func (dns testBIMIDNS) LookupMX(context.Context, string) ([]*net.MX, error) { return nil, nil }

type testWorkspaceAvatars struct {
	fakeWorkspaceProvider
	avatar string
}

func (provider testWorkspaceAvatars) WorkspaceAvatar(context.Context, string) (string, error) {
	return provider.avatar, nil
}

func TestAddressHistoryResolvesBIMIAndSolarPassWorkspaceAvatars(t *testing.T) {
	svc, db, accountID, mailbox, _ := newImportTestService(t)
	mailbox.Address = "user@solarpass.one"
	if err := db.Model(&mailbox).Update("address", mailbox.Address).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&database.Email{
		ID: database.NewID(), AccountID: accountID, MailboxID: mailbox.ID,
		FromAddress: mailbox.Address, Folder: "inbox",
	}).Error; err != nil {
		t.Fatal(err)
	}
	svc.SetWorkspaceProvider(testWorkspaceAvatars{avatar: "https://cdn.example.test/workspace.png"})
	items, err := svc.ListSenders(context.Background(), accountID, "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].AvatarURL != "https://cdn.example.test/workspace.png" || items[0].AvatarSource != "workspace" {
		t.Fatalf("SolarPass sender avatar = %#v", items)
	}

	svc.SetWorkspaceProvider(fakeWorkspaceProvider{})
	svc.dns = testBIMIDNS{records: map[string][]string{
		"default._bimi.example.net": {"v=BIMI1; l=https://cdn.example.net/logo.svg; a="},
	}}
	if err := db.Create(&database.Email{
		ID: database.NewID(), AccountID: accountID, MailboxID: mailbox.ID,
		FromAddress: "person@example.net", Folder: "inbox",
	}).Error; err != nil {
		t.Fatal(err)
	}
	items, err = svc.ListSenders(context.Background(), accountID, "person", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].AvatarURL != "https://cdn.example.net/logo.svg" || items[0].AvatarSource != "bimi" || items[0].BIMIURL != items[0].AvatarURL {
		t.Fatalf("BIMI sender avatar = %#v", items)
	}
}
