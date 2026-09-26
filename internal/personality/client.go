// Package personality calls the Persona service to summarize message content
// for notifications.
package personality

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"

	gen "src.solsynth.dev/sosys/go/proto"
)

const (
	// DefaultTimeout bounds one completion. Notifications are optional
	// enrichment, so a slow agent is skipped rather than delayed.
	DefaultTimeout = 20 * time.Second
	// promptRuneLimit bounds the message content sent to the agent.
	promptRuneLimit = 4000
	// summaryRuneLimit is the notification budget for a summary.
	summaryRuneLimit = 120
	// maxTokens bounds what the agent may spend on one summary. The budget is
	// deliberately roomy for one line so reasoning agents still answer.
	maxTokens = 256
	// temperature keeps summaries factual and reproducible.
	temperature = 0.2
)

// SummaryRequest is one message to summarize. Body is plain text.
type SummaryRequest struct {
	AccountID string
	// Language is the recipient's language tag. An empty value asks the agent
	// to answer in the language of the message.
	Language string
	FromName string
	Subject  string
	Body     string
}

// Summarizer produces a one-line summary of a message for a notification.
type Summarizer interface {
	Summarize(context.Context, SummaryRequest) (string, error)
	Close() error
}

// Config connects to the Persona gRPC endpoint.
type Config struct {
	Target        string
	UseTLS        bool
	TLSSkipVerify bool
	// Agent is the personality agent used for summaries.
	Agent string
	// Model optionally overrides the agent's default model.
	Model string
	// Timeout bounds one summary; zero uses DefaultTimeout.
	Timeout time.Duration
}

// Client summarizes message content through DyPersonalityService.Complete.
type Client struct {
	conn    *grpc.ClientConn
	client  gen.DyPersonalityServiceClient
	agent   string
	model   string
	timeout time.Duration
}

func NewClient(cfg Config) (*Client, error) {
	target := strings.TrimSpace(cfg.Target)
	if target == "" {
		return nil, fmt.Errorf("personality target is required")
	}
	agent := strings.TrimSpace(cfg.Agent)
	if agent == "" {
		return nil, fmt.Errorf("personality agent is required")
	}
	var transport credentials.TransportCredentials
	if cfg.UseTLS {
		transport = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.TLSSkipVerify}) // #nosec G402 -- explicitly configured for internal deployments.
	} else {
		transport = insecure.NewCredentials()
	}
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(transport))
	if err != nil {
		return nil, fmt.Errorf("connect to personality service: %w", err)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Client{conn: conn, client: gen.NewDyPersonalityServiceClient(conn), agent: agent, model: strings.TrimSpace(cfg.Model), timeout: timeout}, nil
}

// Summarize returns a notification-sized summary of the message, or an error
// when the agent is unavailable or answered with nothing usable.
func (c *Client) Summarize(ctx context.Context, request SummaryRequest) (string, error) {
	accountID := strings.TrimSpace(request.AccountID)
	if accountID == "" {
		return "", fmt.Errorf("account id is required")
	}
	timeout := c.timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	completion := &gen.DyCompletePersonalityRequest{
		AgentId:     c.agent,
		AccountId:   accountID,
		Message:     buildPrompt(request),
		Temperature: proto.Float64(temperature),
		MaxTokens:   proto.Int32(maxTokens),
	}
	if c.model != "" {
		completion.Model = proto.String(c.model)
	}
	response, err := c.client.Complete(ctx, completion)
	if err != nil {
		return "", fmt.Errorf("complete summary: %w", err)
	}
	summary := cleanSummary(response.GetContent())
	if summary == "" {
		return "", fmt.Errorf("personality service returned an empty summary")
	}
	return summary, nil
}

func (c *Client) Close() error { return c.conn.Close() }

// buildPrompt asks for one notification-sized line and hands the agent the
// message content it needs, bounded by promptRuneLimit.
func buildPrompt(request SummaryRequest) string {
	language := strings.TrimSpace(request.Language)
	languageRule := "Write the summary in the same language as the email."
	if language != "" {
		languageRule = fmt.Sprintf("Write the summary in %s.", language)
	}
	var prompt strings.Builder
	prompt.WriteString("Summarize this email for a phone notification.\n")
	fmt.Fprintf(&prompt, "Reply with one plain-text line of at most %d characters: no greeting, no quotes, no markdown.\n", summaryRuneLimit)
	prompt.WriteString(languageRule)
	prompt.WriteString("\nSay what the email is about, and what the reader has to do if it asks for anything.\n\n")
	if fromName := strings.TrimSpace(request.FromName); fromName != "" {
		fmt.Fprintf(&prompt, "From: %s\n", fromName)
	}
	if subject := strings.TrimSpace(request.Subject); subject != "" {
		fmt.Fprintf(&prompt, "Subject: %s\n", subject)
	}
	prompt.WriteString("\nEmail body:\n")
	prompt.WriteString(truncate(request.Body, promptRuneLimit))
	return prompt.String()
}

// cleanSummary reduces an agent answer to one notification line: agents often
// wrap a summary in quotes, markdown, or a "Summary:" label.
func cleanSummary(content string) string {
	fields := strings.Fields(strings.TrimSpace(content))
	if len(fields) == 0 {
		return ""
	}
	summary := strings.Join(fields, " ")
	summary = strings.TrimSpace(strings.TrimLeft(summary, "-*•"))
	if at := strings.IndexAny(summary, ":："); at > 0 && isSummaryLabel(summary[:at]) {
		_, size := utf8.DecodeRuneInString(summary[at:])
		summary = strings.TrimSpace(summary[at+size:])
	}
	summary = strings.Trim(summary, "\"'“”‘’`*")
	return truncate(summary, summaryRuneLimit)
}

// isSummaryLabel reports whether a prefix is a label such as "Summary" rather
// than the start of the summary itself.
func isSummaryLabel(prefix string) bool {
	switch strings.ToLower(strings.TrimSpace(prefix)) {
	case "summary", "short summary", "tldr", "tl;dr", "摘要", "总结", "概述":
		return true
	default:
		return false
	}
}

func truncate(value string, limit int) string {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return strings.TrimSpace(string(runes[:limit])) + "…"
}
