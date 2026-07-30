package relay

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"
)

func TestSESAdapterCreatesDomainIdentityWithEasyDKIMRecords(t *testing.T) {
	client := &fakeSESClient{createOutput: &sesv2.CreateEmailIdentityOutput{
		IdentityType:   types.IdentityTypeDomain,
		DkimAttributes: &types.DkimAttributes{Tokens: []string{"one", "two", "three"}},
	}}
	adapter := &SESAdapter{client: client}

	status, err := adapter.CreateIdentity(context.Background(), "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if client.createdIdentity != "example.com" {
		t.Fatalf("identity = %q", client.createdIdentity)
	}
	if status.IdentityType != string(types.IdentityTypeDomain) || len(status.DNSRecords) != 3 {
		t.Fatalf("status = %#v", status)
	}
	if got := status.DNSRecords[0]; got.Name != "one._domainkey.example.com" || got.Value != "one.dkim.amazonses.com" || got.Type != "CNAME" {
		t.Fatalf("DNS record = %#v", got)
	}
}

func TestSESAdapterRefreshesAndDeletesIdentity(t *testing.T) {
	client := &fakeSESClient{getOutput: &sesv2.GetEmailIdentityOutput{
		IdentityType:             types.IdentityTypeDomain,
		VerificationStatus:       types.VerificationStatusSuccess,
		VerifiedForSendingStatus: true,
		DkimAttributes:           &types.DkimAttributes{Status: types.DkimStatusSuccess},
	}}
	adapter := &SESAdapter{client: client}

	status, err := adapter.GetIdentity(context.Background(), "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !status.VerifiedForSendingStatus || status.VerificationStatus != "SUCCESS" || status.DKIMStatus != "SUCCESS" {
		t.Fatalf("status = %#v", status)
	}
	if err := adapter.DeleteIdentity(context.Background(), "example.com"); err != nil {
		t.Fatal(err)
	}
	if client.deletedIdentity != "example.com" {
		t.Fatalf("deleted identity = %q", client.deletedIdentity)
	}
}

type fakeSESClient struct {
	createOutput    *sesv2.CreateEmailIdentityOutput
	getOutput       *sesv2.GetEmailIdentityOutput
	createdIdentity string
	deletedIdentity string
}

func (f *fakeSESClient) SendEmail(context.Context, *sesv2.SendEmailInput, ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error) {
	return &sesv2.SendEmailOutput{}, nil
}
func (f *fakeSESClient) CreateEmailIdentity(_ context.Context, input *sesv2.CreateEmailIdentityInput, _ ...func(*sesv2.Options)) (*sesv2.CreateEmailIdentityOutput, error) {
	f.createdIdentity = aws.ToString(input.EmailIdentity)
	return f.createOutput, nil
}
func (f *fakeSESClient) GetEmailIdentity(context.Context, *sesv2.GetEmailIdentityInput, ...func(*sesv2.Options)) (*sesv2.GetEmailIdentityOutput, error) {
	return f.getOutput, nil
}
func (f *fakeSESClient) DeleteEmailIdentity(_ context.Context, input *sesv2.DeleteEmailIdentityInput, _ ...func(*sesv2.Options)) (*sesv2.DeleteEmailIdentityOutput, error) {
	f.deletedIdentity = aws.ToString(input.EmailIdentity)
	return &sesv2.DeleteEmailIdentityOutput{}, nil
}
