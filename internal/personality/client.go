// Package personality calls the Persona service to summarize message content
// for notifications.
package personality

import (
	"context"
	"crypto/tls"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
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

// IsAccessRejection reports whether the service refused the completion for an
// account-level reason: a usage limit, a missing payment wallet, or a blocked
// account. That is an expected outcome for a background summary rather than a
// failure worth warning about, and the account's own Personality usage and
// billing are what enforce it.
func IsAccessRejection(err error) bool {
	switch status.Code(err) {
	case codes.ResourceExhausted, codes.FailedPrecondition, codes.PermissionDenied:
		return true
	default:
		return false
	}
}

// buildPrompt asks for one notification-sized line and hands the agent the
// message content it needs, bounded by promptRuneLimit. The instructions spend
// most of their words on one failure mode: agents like to report what the email
// is about ("这封邮件是关于…") instead of summarizing what it says.
func buildPrompt(request SummaryRequest) string {
	language := strings.TrimSpace(request.Language)
	languageRule := "Write the summary in the same language as the email."
	if language != "" {
		languageRule = fmt.Sprintf("Write the summary in %s.", language)
	}
	example := summaryExamples["en"]
	if strings.HasPrefix(strings.ToLower(language), "zh") {
		example = summaryExamples["zh"]
	}
	var prompt strings.Builder
	fmt.Fprintf(&prompt, "Summarize what this email says, in one plain-text line of at most %d characters: no greeting, no quotes, no markdown, no label.\n", summaryRuneLimit)
	prompt.WriteString("Summarize the content itself, never the message: do not start with wording like \"this email is about\", \"这封邮件是关于\", \"这是一封关于…的邮件\", and do not describe the email as a document.\n")
	prompt.WriteString("State what the sender is telling or asking the reader, and what the reader has to do if the email asks for anything.\n")
	prompt.WriteString("Write one complete sentence or clause: drop details instead of stopping halfway through a phrase.\n")
	prompt.WriteString(languageRule)
	fmt.Fprintf(&prompt, "\n\nBad: %s\nGood: %s\n", example[0], example[1])
	if fromName := strings.TrimSpace(request.FromName); fromName != "" {
		fmt.Fprintf(&prompt, "\nFrom: %s\n", fromName)
	}
	if subject := strings.TrimSpace(request.Subject); subject != "" {
		fmt.Fprintf(&prompt, "Subject: %s\n", subject)
	}
	prompt.WriteString("\nEmail body:\n")
	prompt.WriteString(truncate(request.Body, promptRuneLimit))
	return prompt.String()
}

// summaryExamples shows the agent the transformation we want, in the language
// the summary must be written in: a bad and good answer to the same email.
var summaryExamples = map[string][2]string{
	"en": {
		"This email is about your order and is a notification from our shop.",
		"Order #A1842 shipped today and arrives on Thursday; nothing to do.",
	},
	"zh": {
		"这封邮件是关于您的订单的，是一封来自商家的通知邮件。",
		"订单 #A1842 今天已发货，预计周四送达，无需操作。",
	},
}

// cleanSummary reduces an agent answer to one notification line: agents often
// wrap a summary in quotes, markdown, or a "Summary:" label, and sometimes open
// by describing the email instead of summarizing it.
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
	summary = stripSummaryPreamble(summary)
	return truncateSummary(summary, summaryRuneLimit)
}

// summaryPreamble matches an opener that describes the email rather than its
// content, such as "This email is about X" or "这封邮件是关于X的". The prompt
// asks for a summary without one; this is the fallback for agents that add one
// anyway.
var summaryPreamble = regexp.MustCompile(`(?i)^(?:(?:this|the|an?)\s+(?:e-?mail|message|mail|letter|notification)\s+(?:is|was|seems)?\s*(?:about|regarding|concerning|announces?|announcing|informs?[^,，:：]*|says|describes|contains|covers|discusses|invites?|reminds?)\s*[:：,，]?\s*|(?:这|本|该|那)?\s*(?:封|条)?\s*(?:电子邮件|电邮|邮件内容|邮件|信件|消息|通知)\s*(?:是|为|讲的|说的|谈的|讲的是|说的是|谈的是|内容为|内容是|主要是|是有关|是关于)\s*(?:(?:一篇|一封|一个|一则|一条|一段)?(?:关于|有关)?|讲述|介绍|说明|宣布|通知|邀请)?\s*)`)

// stripSummaryPreamble removes that opener, dropping the dangling 的 that
// Chinese openers such as "这封邮件是关于…的" leave behind.
func stripSummaryPreamble(summary string) string {
	location := summaryPreamble.FindStringIndex(summary)
	if location == nil || location[0] != 0 {
		return summary
	}
	rest := strings.TrimSpace(summary[location[1]:])
	if rest == "" {
		// The agent answered with the opener alone: keep it rather than
		// returning nothing.
		return summary
	}
	if strings.HasPrefix(summary, "这") || strings.HasPrefix(summary, "本") || strings.HasPrefix(summary, "该") {
		rest = strings.TrimSuffix(rest, "的")
	}
	return upperFirstRune(rest)
}

// upperFirstRune capitalizes a Latin opening letter left behind by a stripped
// opener; other scripts are returned unchanged.
func upperFirstRune(value string) string {
	first, size := utf8.DecodeRuneInString(value)
	if first < 'a' || first > 'z' {
		return value
	}
	return string(first-'a'+'A') + value[size:]
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

// truncateSummary bounds a summary to the notification budget, preferring to
// end on a clause break so the reader is not handed half a phrase.
func truncateSummary(value string, limit int) string {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)[:limit]
	// Only a break near the budget is worth taking: an earlier one would drop
	// too much of what the agent wrote.
	if at := lastClauseBreak(runes); at > limit-limit/4 {
		runes = runes[:at]
	}
	return strings.TrimRight(strings.TrimSpace(string(runes)), " ,，、;；:：.。!！?？") + "…"
}

// lastClauseBreak returns the offset just after the last sentence or clause
// break in the runes.
func lastClauseBreak(runes []rune) int {
	for index := len(runes) - 1; index >= 0; index-- {
		switch runes[index] {
		case '.', '!', '?', ',', ';', ':', '。', '！', '？', '，', '、', '；', '：':
			return index + 1
		}
	}
	return 0
}

func truncate(value string, limit int) string {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return strings.TrimSpace(string(runes[:limit])) + "…"
}
