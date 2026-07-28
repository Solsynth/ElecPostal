package relay

import (
	"context"
	"fmt"
	"net"
	"testing"
)

func TestDirectSMTPAdapterDeliversLocallyWhenMXMatchesHostname(t *testing.T) {
	adapter, err := NewDirectSMTPAdapter(DirectSMTPConfig{
		Hostname:    "mail.example.com",
		InboundHost: "mail.example.com",
		LocalDelivery: func(_ context.Context, message Message, recipients []string) error {
			if message.Subject != "local" {
				t.Fatalf("subject = %q, want local", message.Subject)
			}
			if len(recipients) != 1 || recipients[0] != "user@example.com" {
				t.Fatalf("recipients = %#v", recipients)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter.lookupMX = func(context.Context, string) ([]*net.MX, error) {
		return []*net.MX{{Host: "MAIL.EXAMPLE.COM.", Pref: 10}}, nil
	}
	adapter.dial = func(context.Context, string, string) (net.Conn, error) {
		return nil, fmt.Errorf("SMTP dial must not occur for a local MX")
	}

	_, err = adapter.Send(context.Background(), Message{
		FromAddress: "sender@elsewhere.test",
		To:          []string{"user@example.com"},
		Subject:     "local",
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestHasLocalMX(t *testing.T) {
	if !hasLocalMX([]*net.MX{{Host: "mail.example.com."}}, "MAIL.EXAMPLE.COM") {
		t.Fatal("expected normalized MX hostname to match")
	}
	if hasLocalMX([]*net.MX{{Host: "remote.example.com."}}, "mail.example.com") {
		t.Fatal("unexpected local MX match")
	}
}
