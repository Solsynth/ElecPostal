package relay

import (
	"context"
	"net"
	"strings"
)

// LocalMXRouter delivers domains served by InboundHost directly and delegates
// all other recipients to Fallback. DNS lookup failures use Fallback, which is
// important for providers such as SES that resolve recipient MX records on the
// caller's behalf.
type LocalMXRouter struct {
	inboundHost   string
	localDelivery func(context.Context, Message, []string) error
	fallback      Adapter
	lookupMX      func(context.Context, string) ([]*net.MX, error)
}

func NewLocalMXRouter(inboundHost string, localDelivery func(context.Context, Message, []string) error, fallback Adapter) *LocalMXRouter {
	return &LocalMXRouter{
		inboundHost:   inboundHost,
		localDelivery: localDelivery,
		fallback:      fallback,
		lookupMX:      net.DefaultResolver.LookupMX,
	}
}

func (r *LocalMXRouter) Send(ctx context.Context, message Message) error {
	if r.localDelivery == nil || strings.TrimSpace(r.inboundHost) == "" {
		return r.fallback.Send(ctx, message)
	}
	recipients, err := envelopeRecipients(message)
	if err != nil {
		return err
	}
	byDomain := make(map[string][]string)
	for _, recipient := range recipients {
		parts := strings.Split(recipient, "@")
		domain := strings.ToLower(parts[len(parts)-1])
		byDomain[domain] = append(byDomain[domain], recipient)
	}

	localRecipients := make(map[string]struct{})
	for domain, domainRecipients := range byDomain {
		mxRecords, err := r.lookupMX(ctx, domain)
		if err != nil || !hasLocalMX(mxRecords, r.inboundHost) {
			continue
		}
		if err := r.localDelivery(ctx, message, domainRecipients); err != nil {
			return err
		}
		for _, recipient := range domainRecipients {
			localRecipients[strings.ToLower(recipient)] = struct{}{}
		}
	}

	external := excludeRecipients(message, localRecipients)
	if len(external.To)+len(external.Cc)+len(external.Bcc) == 0 {
		return nil
	}
	return r.fallback.Send(ctx, external)
}

func (r *LocalMXRouter) Close() error { return r.fallback.Close() }

func excludeRecipients(message Message, excluded map[string]struct{}) Message {
	message.To = filterRecipients(message.To, excluded)
	message.Cc = filterRecipients(message.Cc, excluded)
	message.Bcc = filterRecipients(message.Bcc, excluded)
	return message
}

func filterRecipients(recipients []string, excluded map[string]struct{}) []string {
	filtered := make([]string, 0, len(recipients))
	for _, recipient := range recipients {
		if _, ok := excluded[strings.ToLower(recipient)]; !ok {
			filtered = append(filtered, recipient)
		}
	}
	return filtered
}
