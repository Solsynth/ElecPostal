package config

import "testing"

// TestDefaultMailSpam pins the spam filter defaults published in
// config.example.toml: the filter and its header injection are on, the routing
// threshold is 5.0, and Bayes requires 100 learns per class.
func TestDefaultMailSpam(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	spam := cfg.Mail.Spam
	if !spam.Enabled {
		t.Fatal("mail.spam.enabled default = false, want true")
	}
	if spam.Threshold != 5.0 {
		t.Fatalf("mail.spam.threshold default = %v, want 5.0", spam.Threshold)
	}
	if !spam.AddXSpamHeader {
		t.Fatal("mail.spam.addXSpamHeader default = false, want true")
	}
	if spam.Segmenter != "gse" {
		t.Fatalf("mail.spam.segmenter default = %q, want gse", spam.Segmenter)
	}
	if !spam.Bayes.Enabled {
		t.Fatal("mail.spam.bayes.enabled default = false, want true")
	}
	if spam.Bayes.MinLearns != 100 {
		t.Fatalf("mail.spam.bayes.minLearns default = %d, want 100", spam.Bayes.MinLearns)
	}
	if spam.Bayes.MinTokens != 11 {
		t.Fatalf("mail.spam.bayes.minTokens default = %d, want 11", spam.Bayes.MinTokens)
	}
}
