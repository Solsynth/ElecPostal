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
