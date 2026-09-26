package personality

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	gen "src.solsynth.dev/sosys/go/proto"
)

type fakePersonalityService struct {
	gen.UnimplementedDyPersonalityServiceServer
	requests []*gen.DyCompletePersonalityRequest
	content  string
	err      error
	block    time.Duration
}

func (f *fakePersonalityService) Complete(ctx context.Context, req *gen.DyCompletePersonalityRequest) (*gen.DyCompletePersonalityResponse, error) {
	f.requests = append(f.requests, req)
	if f.err != nil {
		return nil, f.err
	}
	if f.block > 0 {
		select {
		case <-time.After(f.block):
		case <-ctx.Done():
			return nil, status.Error(codes.DeadlineExceeded, "context deadline exceeded")
		}
	}
	return &gen.DyCompletePersonalityResponse{Content: f.content, Model: "test-model"}, nil
}

func newTestClient(t *testing.T, cfg Config, service *fakePersonalityService) *Client {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	gen.RegisterDyPersonalityServiceServer(server, service)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient() error = %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &Client{conn: conn, client: gen.NewDyPersonalityServiceClient(conn), agent: cfg.Agent, model: cfg.Model, timeout: cfg.Timeout}
}

func TestSummarizeSendsAgentContextAndBoundsInput(t *testing.T) {
	service := &fakePersonalityService{content: "Your parcel arrives on Friday"}
	client := newTestClient(t, Config{Agent: "michan", Model: "test-model"}, service)

	summary, err := client.Summarize(context.Background(), SummaryRequest{
		AccountID: "account-1",
		Language:  "zh-CN",
		FromName:  "Acme Billing",
		Subject:   "Your invoice",
		Body:      strings.Repeat("长", promptRuneLimit*2),
	})
	if err != nil {
		t.Fatalf("Summarize() error = %v", err)
	}
	if summary != "Your parcel arrives on Friday" {
		t.Fatalf("Summarize() = %q, want the agent answer", summary)
	}
	if len(service.requests) != 1 {
		t.Fatalf("completions sent = %d, want 1", len(service.requests))
	}
	request := service.requests[0]
	if request.GetAgentId() != "michan" || request.GetAccountId() != "account-1" {
		t.Fatalf("agent/account = %q/%q, want michan/account-1", request.GetAgentId(), request.GetAccountId())
	}
	if request.GetModel() != "test-model" {
		t.Fatalf("model = %q, want test-model", request.GetModel())
	}
	if request.GetTemperature() != temperature || request.GetMaxTokens() != maxTokens {
		t.Fatalf("temperature/max_tokens = %v/%v, want %v/%d", request.GetTemperature(), request.GetMaxTokens(), temperature, maxTokens)
	}
	prompt := request.GetMessage()
	for _, want := range []string{"zh-CN", "Acme Billing", "Your invoice", "Email body:"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt is missing %q:\n%s", want, prompt)
		}
	}
	// The body is the only unbounded part of the prompt, so the prompt ends
	// with it cut to promptRuneLimit.
	if body := truncate(strings.Repeat("长", promptRuneLimit*2), promptRuneLimit); !strings.HasSuffix(prompt, body) {
		t.Fatalf("prompt should end with the body bounded by %d runes", promptRuneLimit)
	}
}

func TestBuildPromptAsksForTheContentNotTheEmail(t *testing.T) {
	prompt := buildPrompt(SummaryRequest{Language: "zh-CN", Subject: "冰淇淋迷因"})
	for _, want := range []string{
		`do not start with wording like "this email is about", "这封邮件是关于"`,
		"drop details instead of stopping halfway",
		summaryExamples["zh"][0],
		summaryExamples["zh"][1],
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt is missing %q:\n%s", want, prompt)
		}
	}
	english := buildPrompt(SummaryRequest{Language: "en-US"})
	if !strings.Contains(english, summaryExamples["en"][1]) || strings.Contains(english, summaryExamples["zh"][1]) {
		t.Fatalf("english prompt should show the english example:\n%s", english)
	}
	// An unknown language cannot be matched, so the agent gets the English
	// example and the same-language rule.
	unknown := buildPrompt(SummaryRequest{})
	if !strings.Contains(unknown, summaryExamples["en"][1]) || !strings.Contains(unknown, "same language as the email") {
		t.Fatalf("unmatched language prompt = %q", unknown)
	}
}

func TestCleanSummaryStripsEmailPreamble(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "chinese about-opener",
			content: "这封邮件是关于约翰塞纳冰淇淋迷因与冰淇淋文化象征的",
			want:    "约翰塞纳冰淇淋迷因与冰淇淋文化象征",
		},
		{
			name:    "chinese opener with trailing clause",
			content: "邮件是一篇关于约翰塞纳冰淇淋迷因的文章，讲了他如何成为文化符号",
			want:    "约翰塞纳冰淇淋迷因的文章，讲了他如何成为文化符号",
		},
		{
			name:    "chinese opener alone",
			content: "这封邮件是关于",
			want:    "这封邮件是关于",
		},
		{
			name:    "english about-opener",
			content: "This email is about your upcoming invoice.",
			want:    "Your upcoming invoice.",
		},
		{
			name:    "english opener with colon",
			content: "The message is about: a 10% price increase.",
			want:    "A 10% price increase.",
		},
		{
			name:    "content that starts with a subject line",
			content: "邮件已发送失败，请检查收件地址",
			want:    "邮件已发送失败，请检查收件地址",
		},
		{
			name:    "content that mentions the email later",
			content: "Acme 的报价单将在周五到期，这封邮件里附了明细",
			want:    "Acme 的报价单将在周五到期，这封邮件里附了明细",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := cleanSummary(test.content); got != test.want {
				t.Fatalf("cleanSummary(%q) = %q, want %q", test.content, got, test.want)
			}
		})
	}
}

func TestTruncateSummaryStopsAtAClauseBreak(t *testing.T) {
	long := strings.Repeat("这一条消息说明了发货安排与配送进度,", 12) + "这一条消息说明了一切"
	got := truncateSummary(long, summaryRuneLimit)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncateSummary() = %q, want an ellipsis", got)
	}
	if strings.HasSuffix(strings.TrimSuffix(got, "…"), ",") {
		t.Fatalf("truncateSummary() = %q, want the clause break dropped", got)
	}
	if runes := len([]rune(got)); runes > summaryRuneLimit+1 {
		t.Fatalf("truncateSummary() = %d runes, want at most %d", runes, summaryRuneLimit)
	}
	if body := strings.Repeat("字", summaryRuneLimit+10); truncateSummary(body, summaryRuneLimit) == body {
		t.Fatal("truncateSummary() should bound a summary with no clause break")
	}
}

func TestSummarizeReportsFailures(t *testing.T) {
	t.Run("agent error", func(t *testing.T) {
		client := newTestClient(t, Config{Agent: "michan"}, &fakePersonalityService{err: status.Error(codes.NotFound, "agent not found")})
		if _, err := client.Summarize(context.Background(), SummaryRequest{AccountID: "account-1", Subject: "hi"}); err == nil {
			t.Fatal("Summarize() error = nil, want the agent error")
		}
	})
	t.Run("empty answer", func(t *testing.T) {
		client := newTestClient(t, Config{Agent: "michan"}, &fakePersonalityService{content: "   "})
		if _, err := client.Summarize(context.Background(), SummaryRequest{AccountID: "account-1", Subject: "hi"}); err == nil {
			t.Fatal("Summarize() error = nil, want an error for an empty answer")
		}
	})
	t.Run("missing account", func(t *testing.T) {
		client := newTestClient(t, Config{Agent: "michan"}, &fakePersonalityService{content: "summary"})
		if _, err := client.Summarize(context.Background(), SummaryRequest{Subject: "hi"}); err == nil {
			t.Fatal("Summarize() error = nil, want an error for a missing account id")
		}
	})
	t.Run("timeout", func(t *testing.T) {
		client := newTestClient(t, Config{Agent: "michan", Timeout: 20 * time.Millisecond}, &fakePersonalityService{content: "late", block: time.Second})
		if _, err := client.Summarize(context.Background(), SummaryRequest{AccountID: "account-1", Subject: "hi"}); err == nil {
			t.Fatal("Summarize() error = nil, want a timeout error")
		}
	})
}

func TestSummarizeReturnsTheContentWhenTheAgentDescribesTheEmail(t *testing.T) {
	service := &fakePersonalityService{content: "这封邮件是关于约翰塞纳冰淇淋迷因与冰淇淋文化象征的"}
	client := newTestClient(t, Config{Agent: "michan"}, service)

	summary, err := client.Summarize(context.Background(), SummaryRequest{
		AccountID: "account-1", Language: "zh-CN", Subject: "冰淇淋迷因",
		Body: "本周约翰·塞纳的「冰淇淋」片段走红。",
	})
	if err != nil {
		t.Fatalf("Summarize() error = %v", err)
	}
	if want := "约翰塞纳冰淇淋迷因与冰淇淋文化象征"; summary != want {
		t.Fatalf("Summarize() = %q, want %q", summary, want)
	}
}

func TestNewClientRequiresTargetAndAgent(t *testing.T) {
	if _, err := NewClient(Config{Agent: "michan"}); err == nil {
		t.Fatal("NewClient() error = nil, want an error for a missing target")
	}
	if _, err := NewClient(Config{Target: "persona:9095"}); err == nil {
		t.Fatal("NewClient() error = nil, want an error for a missing agent")
	}
}

func TestCleanSummaryReducesAgentNoise(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "label and quotes", content: `Summary: "Your parcel arrives on Friday"`, want: "Your parcel arrives on Friday"},
		{name: "markdown bullet", content: "- **Invoice 4482 is past due**", want: "Invoice 4482 is past due"},
		{name: "wrapped lines", content: "Your parcel arrives\n   on Friday", want: "Your parcel arrives on Friday"},
		{name: "chinese label", content: "摘要：您的包裹将于周五送达", want: "您的包裹将于周五送达"},
		{name: "empty", content: " \n\t ", want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := cleanSummary(test.content); got != test.want {
				t.Fatalf("cleanSummary(%q) = %q, want %q", test.content, got, test.want)
			}
		})
	}
}

func TestCleanSummaryIsBounded(t *testing.T) {
	got := cleanSummary(strings.Repeat("word ", 100))
	if runes := []rune(got); len(runes) > summaryRuneLimit+1 {
		t.Fatalf("summary length = %d runes, want at most %d plus ellipsis", len(runes), summaryRuneLimit)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("summary = %q, want a truncated line", got)
	}
}

func TestIsAccessRejectionClassifiesAccountLevelRefusals(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "usage limit", err: status.Error(codes.ResourceExhausted, "usage limit is 5"), want: true},
		{name: "wrapped usage limit", err: fmt.Errorf("complete summary: %w", status.Error(codes.ResourceExhausted, "usage limit is 5")), want: true},
		{name: "payment wallet", err: status.Error(codes.FailedPrecondition, "a payment wallet is required"), want: true},
		{name: "blocked account", err: status.Error(codes.PermissionDenied, "blocked"), want: true},
		{name: "agent failure", err: status.Error(codes.NotFound, `agent "mail-summarizer" is unavailable`), want: false},
		{name: "timeout", err: status.Error(codes.DeadlineExceeded, "deadline exceeded"), want: false},
		{name: "transport error", err: errors.New("connection refused"), want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsAccessRejection(test.err); got != test.want {
				t.Fatalf("IsAccessRejection(%v) = %v, want %v", test.err, got, test.want)
			}
		})
	}
}
