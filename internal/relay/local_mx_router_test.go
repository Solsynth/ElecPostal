package relay

import (
	"context"
	"net"
	"testing"
)

func TestLocalMXRouterDeliversLocalRecipientsAndRelaysTheRest(t *testing.T) {
	var local []string
	var relayed Message
	fallback := adapterFunc(func(_ context.Context, message Message) error {
		relayed = message
		return nil
	})
	router := NewLocalMXRouter("mail.example.com", func(_ context.Context, _ Message, recipients []string) error {
		local = append(local, recipients...)
		return nil
	}, fallback)
	router.lookupMX = func(_ context.Context, domain string) ([]*net.MX, error) {
		if domain == "local.test" {
			return []*net.MX{{Host: "mail.example.com."}}, nil
		}
		return []*net.MX{{Host: "remote.test."}}, nil
	}

	if err := router.Send(context.Background(), Message{
		FromAddress: "sender@example.test",
		To:          []string{"local@local.test", "remote@remote.test"},
	}); err != nil {
		t.Fatal(err)
	}
	if len(local) != 1 || local[0] != "local@local.test" {
		t.Fatalf("local recipients = %#v", local)
	}
	if len(relayed.To) != 1 || relayed.To[0] != "remote@remote.test" {
		t.Fatalf("relayed recipients = %#v", relayed.To)
	}
}

type adapterFunc func(context.Context, Message) error

func (f adapterFunc) Send(ctx context.Context, message Message) error { return f(ctx, message) }
func (adapterFunc) Close() error                                      { return nil }
