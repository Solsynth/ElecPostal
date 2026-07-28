package relay

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
	"sort"
	"strings"
)

// DirectSMTPConfig controls direct-to-MX delivery. STARTTLS is attempted when
// advertised by the receiving server; RequireTLS makes that mandatory.
type DirectSMTPConfig struct {
	Hostname       string
	RequireTLS     bool
	TLSSkipVerify  bool
	ConnectTimeout int // Reserved for a dialer-backed adapter implementation.
}

// DirectSMTPAdapter delivers mail directly to each recipient domain's MX host.
// It does not use a third-party relay and therefore needs outbound TCP/25.
type DirectSMTPAdapter struct {
	cfg DirectSMTPConfig
}

func NewDirectSMTPAdapter(cfg DirectSMTPConfig) (*DirectSMTPAdapter, error) {
	if strings.TrimSpace(cfg.Hostname) == "" {
		return nil, fmt.Errorf("direct SMTP hostname is required")
	}
	return &DirectSMTPAdapter{cfg: cfg}, nil
}

func (a *DirectSMTPAdapter) Send(ctx context.Context, message Message) error {
	if len(message.AttachmentIDs) > 0 {
		return ErrAttachmentSourceRequired
	}
	from, err := mail.ParseAddress(message.FromAddress)
	if err != nil || from.Address == "" {
		return fmt.Errorf("invalid sender address %q", message.FromAddress)
	}
	recipients, err := envelopeRecipients(message)
	if err != nil {
		return err
	}
	byDomain := make(map[string][]string)
	for _, recipient := range recipients {
		parts := strings.Split(recipient, "@")
		byDomain[strings.ToLower(parts[len(parts)-1])] = append(byDomain[strings.ToLower(parts[len(parts)-1])], recipient)
	}
	data := formatMessage(message)
	for domain, domainRecipients := range byDomain {
		if err := a.deliverDomain(ctx, from.Address, domain, domainRecipients, data); err != nil {
			return fmt.Errorf("deliver to %s: %w", domain, err)
		}
	}
	return nil
}

func (a *DirectSMTPAdapter) deliverDomain(ctx context.Context, from, domain string, recipients []string, data []byte) error {
	mxRecords, err := net.DefaultResolver.LookupMX(ctx, domain)
	if err != nil {
		return err
	}
	sort.Slice(mxRecords, func(i, j int) bool { return mxRecords[i].Pref < mxRecords[j].Pref })
	var lastErr error
	for _, mx := range mxRecords {
		host := strings.TrimSuffix(mx.Host, ".")
		connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(host, "25"))
		if err != nil {
			lastErr = err
			continue
		}
		client, err := smtp.NewClient(connection, a.cfg.Hostname)
		if err == nil {
			if supportsTLS, _ := client.Extension("STARTTLS"); supportsTLS {
				err = client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, InsecureSkipVerify: a.cfg.TLSSkipVerify}) // #nosec G402 -- explicitly configured for internal deployments.
			} else if a.cfg.RequireTLS {
				err = fmt.Errorf("recipient MX does not support STARTTLS")
			}
		}
		if err == nil {
			err = sendSMTPTransaction(client, from, recipients, data)
		}
		if client != nil {
			_ = client.Close()
		}
		if err == nil {
			return nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no MX records for %s", domain)
	}
	return lastErr
}

func sendSMTPTransaction(client *smtp.Client, from string, recipients []string, data []byte) error {
	if err := client.Mail(from); err != nil {
		return err
	}
	for _, recipient := range recipients {
		if err := client.Rcpt(recipient); err != nil {
			return err
		}
	}
	writer, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := writer.Write(data); err != nil {
		_ = writer.Close()
		return err
	}
	return writer.Close()
}

func envelopeRecipients(message Message) ([]string, error) {
	addresses := append(append(append([]string{}, message.To...), message.Cc...), message.Bcc...)
	if len(addresses) == 0 {
		return nil, fmt.Errorf("at least one recipient is required")
	}
	result := make([]string, 0, len(addresses))
	for _, raw := range addresses {
		address, err := mail.ParseAddress(raw)
		if err != nil || address.Address == "" || !strings.Contains(address.Address, "@") {
			return nil, fmt.Errorf("invalid recipient address %q", raw)
		}
		result = append(result, address.Address)
	}
	return result, nil
}

func formatMessage(message Message) []byte {
	var buffer bytes.Buffer
	buffer.WriteString("From: ")
	buffer.WriteString(message.FromAddress)
	buffer.WriteString("\r\nTo: ")
	buffer.WriteString(strings.Join(message.To, ", "))
	if len(message.Cc) > 0 {
		buffer.WriteString("\r\nCc: ")
		buffer.WriteString(strings.Join(message.Cc, ", "))
	}
	buffer.WriteString("\r\nSubject: ")
	buffer.WriteString(message.Subject)
	buffer.WriteString("\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n")
	buffer.WriteString(message.Body)
	return buffer.Bytes()
}

func (a *DirectSMTPAdapter) Close() error { return nil }
