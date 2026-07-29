package config

import "testing"

func TestDefaultMailSendLimits(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.Mail.SendLimits.Free; got.MailboxDaily != 100 || got.MailboxMonthly != 2000 || got.WorkspaceDaily != 100 || got.WorkspaceMonthly != 2000 {
		t.Fatalf("unexpected free limits: %+v", got)
	}
	if got := cfg.Mail.SendLimits.Pro; got.MailboxDaily != 1000 || got.MailboxMonthly != 20000 || got.WorkspaceDaily != 3000 || got.WorkspaceMonthly != 60000 {
		t.Fatalf("unexpected pro limits: %+v", got)
	}
	if got := cfg.Mail.SendLimits.Enterprise; got.MailboxDaily != 5000 || got.MailboxMonthly != 100000 || got.WorkspaceDaily != 25000 || got.WorkspaceMonthly != 500000 {
		t.Fatalf("unexpected enterprise limits: %+v", got)
	}
}
