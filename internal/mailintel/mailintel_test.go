package mailintel

import (
	"strings"
	"testing"
)

func TestAnalyzePicksVerificationCodes(t *testing.T) {
	tests := []struct {
		name        string
		subject     string
		body        string
		contentType string
		wantKind    Kind
		wantText    string
		wantCode    string
	}{
		{
			name:     "english body sentence",
			subject:  "Your GitHub verification code",
			body:     "Your code is 123456. It expires in 10 minutes.",
			wantKind: KindCode, wantText: "123456", wantCode: "123456",
		},
		{
			name:     "code on its own line below the introducing sentence",
			subject:  "Sign in to Acme",
			body:     "Hi Ada,\n\nYour verification code is shown below.\n\n482913\n\nThanks,\nAcme",
			wantKind: KindCode, wantText: "482913", wantCode: "482913",
		},
		{
			name:        "html body",
			subject:     "Verify your email",
			contentType: "text/html",
			body:        `<html><body><p>Enter the confirmation code:</p><h1>774 201</h1><p>Thanks!</p></body></html>`,
			wantKind:    KindCode, wantText: "774201", wantCode: "774201",
		},
		{
			name:     "hyphenated digits",
			subject:  "Acme security code",
			body:     "Your security code is 882-149.",
			wantKind: KindCode, wantText: "882149", wantCode: "882149",
		},
		{
			name:     "alphanumeric one-time password",
			subject:  "Your one-time password",
			body:     "Use one-time password A1B2C3 to sign in.",
			wantKind: KindCode, wantText: "A1B2C3", wantCode: "A1B2C3",
		},
		{
			name:     "chinese verification code",
			subject:  "登录验证",
			body:     "您的验证码为 638415，请在 10 分钟内输入。",
			wantKind: KindCode, wantText: "638415", wantCode: "638415",
		},
		{
			name:     "chinese spaced code",
			subject:  "验证邮件",
			body:     "验证码：123 456，请勿泄露。",
			wantKind: KindCode, wantText: "123456", wantCode: "123456",
		},
		{
			name:     "code in subject",
			subject:  "Code 310277 is your login code",
			body:     "This code expires in 5 minutes.",
			wantKind: KindCode, wantText: "310277", wantCode: "310277",
		},
		{
			name:     "promo code is an offer, not a credential",
			subject:  "20% off your next order",
			body:     "Use code SAVE20 at checkout. Offer expires Friday.",
			wantKind: KindAction, wantText: "Use code SAVE20 at checkout", wantCode: "SAVE20",
		},
		{
			name:     "chinese promo code",
			subject:  "限时优惠",
			body:     "结算时使用优惠码 DISCOUNT50 即可立减。活动即将过期。",
			wantKind: KindAction, wantText: "结算时使用优惠码 DISCOUNT50 即可立减", wantCode: "DISCOUNT50",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Analyze(test.subject, test.body, test.contentType)
			if got.Kind != test.wantKind {
				t.Fatalf("kind = %q, want %q (highlight %+v)", got.Kind, test.wantKind, got)
			}
			if got.Text != test.wantText {
				t.Fatalf("text = %q, want %q", got.Text, test.wantText)
			}
			if got.Code != test.wantCode {
				t.Fatalf("code = %q, want %q", got.Code, test.wantCode)
			}
		})
	}
}

func TestAnalyzePicksSecurityAndActionExcerpts(t *testing.T) {
	tests := []struct {
		name     string
		subject  string
		body     string
		wantKind Kind
		wantText string
	}{
		{
			name:     "security alert from the subject",
			subject:  "New sign-in to your Acme account",
			body:     "We noticed a new sign-in from Berlin. If this was not you, reset your password.",
			wantKind: KindSecurity, wantText: "We noticed a new sign-in from Berlin",
		},
		{
			name:     "security alert from the body",
			subject:  "Acme account notification",
			body:     "Hello Ada,\nYour password was changed on 2026-09-26.\nIf you did not do this, contact support.",
			wantKind: KindSecurity, wantText: "Your password was changed on 2026-09-26",
		},
		{
			name:     "action request from the subject",
			subject:  "Confirm your email address",
			body:     "Please click the button below to confirm your address.",
			wantKind: KindAction, wantText: "Please click the button below to confirm your address",
		},
		{
			name:     "action request from the body",
			subject:  "Your Acme invoice",
			body:     "Hello,\nInvoice 4482 is now past due. Please update your payment method.",
			wantKind: KindAction, wantText: "Invoice 4482 is now past due",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Analyze(test.subject, test.body, "text/plain")
			if got.Kind != test.wantKind {
				t.Fatalf("kind = %q, want %q (highlight %+v)", got.Kind, test.wantKind, got)
			}
			if got.Text != test.wantText {
				t.Fatalf("text = %q, want %q", got.Text, test.wantText)
			}
		})
	}
}

func TestAnalyzeLeavesOrdinaryMailAlone(t *testing.T) {
	tests := []struct {
		name    string
		subject string
		body    string
	}{
		{
			name:    "newsletter",
			subject: "This week at Acme",
			body:    "Hello Ada, here is what shipped this week. Enjoy the read.",
		},
		{
			name:    "shipping notice with a zip code",
			subject: "Your order is on the way",
			body:    "Hello Ada, your parcel left the warehouse. Deliver to zip code 94107 by Friday.",
		},
		{
			name:    "tracking numbers are not codes",
			subject: "Shipment update",
			body:    "Tracking code 1Z999AA10123456784 is in transit. Find it on our site.",
		},
		{
			name:    "dates are not codes",
			subject: "Your statement is ready",
			body:    "Your statement for 2026-09-26 is attached. Some numbers: 20260101 was processed.",
		},
		{
			name:    "unrelated numbers",
			subject: "Team lunch",
			body:    "Table for 4 at 12:30, we booked 2026 rooms in the hotel.",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Analyze(test.subject, test.body, "text/plain")
			if got.Kind != KindNone {
				t.Fatalf("kind = %q, want none (highlight %+v)", got.Kind, got)
			}
			if got.Text != "" || got.Code != "" {
				t.Fatalf("highlight = %+v, want zero value", got)
			}
		})
	}
}

func TestAnalyzeExcerptIsBounded(t *testing.T) {
	got := Analyze("Action required", "Please confirm the following details before we can continue with this unusually long request that keeps going and going and going far beyond what a notification can show.", "text/plain")
	if got.Kind != KindAction {
		t.Fatalf("kind = %q, want action", got.Kind)
	}
	if runes := []rune(got.Text); len(runes) > excerptRuneLimit+1 {
		t.Fatalf("text length = %d runes, want at most %d plus ellipsis", len(runes), excerptRuneLimit)
	}
	if !strings.HasSuffix(got.Text, "…") {
		t.Fatalf("text = %q, want a truncated excerpt", got.Text)
	}
}
