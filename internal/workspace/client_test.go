package workspace

import (
	"testing"

	gen "src.solsynth.dev/sosys/go/proto"
)

func TestMailboxLimitForPlan(t *testing.T) {
	tests := []struct {
		plan gen.DyWorkspacePlan
		want int64
	}{
		{gen.DyWorkspacePlan_FREE, 1},
		{gen.DyWorkspacePlan_PRO, 3},
		{gen.DyWorkspacePlan_ENTERPRISE, 10},
	}
	for _, test := range tests {
		if got := mailboxLimitForPlan(test.plan); got != test.want {
			t.Errorf("mailboxLimitForPlan(%s) = %d, want %d", test.plan, got, test.want)
		}
	}
}

func TestCustomDomainLimitForPlan(t *testing.T) {
	tests := []struct {
		plan gen.DyWorkspacePlan
		want int64
	}{
		{gen.DyWorkspacePlan_FREE, 0},
		{gen.DyWorkspacePlan_PRO, 1},
		{gen.DyWorkspacePlan_ENTERPRISE, 3},
	}
	for _, test := range tests {
		if got := customDomainLimitForPlan(test.plan); got != test.want {
			t.Errorf("customDomainLimitForPlan(%s) = %d, want %d", test.plan, got, test.want)
		}
	}
}

func TestDefaultSendLimitPolicy(t *testing.T) {
	policy := DefaultSendLimitPolicy()
	if got := policy.Free; got.MailboxDaily != 100 || got.MailboxMonthly != 2000 || got.WorkspaceDaily != 100 || got.WorkspaceMonthly != 2000 {
		t.Fatalf("unexpected free send limits: %+v", got)
	}
	if got := policy.Pro; got.MailboxDaily != 1000 || got.MailboxMonthly != 20000 || got.WorkspaceDaily != 3000 || got.WorkspaceMonthly != 60000 {
		t.Fatalf("unexpected pro send limits: %+v", got)
	}
	if got := policy.Enterprise; got.MailboxDaily != 5000 || got.MailboxMonthly != 100000 || got.WorkspaceDaily != 25000 || got.WorkspaceMonthly != 500000 {
		t.Fatalf("unexpected enterprise send limits: %+v", got)
	}
}

func TestStorageBytesFromQuota(t *testing.T) {
	tests := []struct {
		name  string
		quota *gen.DyWorkspacePlanQuota
		want  int64
		ok    bool
	}{
		{
			name:  "configured",
			quota: &gen.DyWorkspacePlanQuota{Quotas: map[string]int64{"max_storage_bytes": 10 * 1024 * 1024 * 1024}},
			want:  10 * 1024 * 1024 * 1024,
			ok:    true,
		},
		{name: "missing", quota: &gen.DyWorkspacePlanQuota{}, ok: false},
		{
			name:  "zero",
			quota: &gen.DyWorkspacePlanQuota{Quotas: map[string]int64{"max_storage_bytes": 0}},
			ok:    false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := storageBytesFromQuota(test.quota)
			if got != test.want || ok != test.ok {
				t.Fatalf("storageBytesFromQuota() = (%d, %t), want (%d, %t)", got, ok, test.want, test.ok)
			}
		})
	}
}
