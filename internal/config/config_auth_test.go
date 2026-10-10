package config

import "testing"

// The development identity header is opt-in: it must default off and only turn
// on when the operator asks for it.
func TestAuthAllowDevIDsDefaultsOffAndHonoursEnv(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Auth.AllowDevIDs {
		t.Fatalf("AllowDevIDs default = true, want false")
	}

	t.Setenv("AUTH_ALLOW_DEV_IDS", "true")
	cfg, err = Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.Auth.AllowDevIDs {
		t.Fatalf("AllowDevIDs with AUTH_ALLOW_DEV_IDS=true = false, want true")
	}
}
