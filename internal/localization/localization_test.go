package localization

import "testing"

func TestLocalizeUsesLocaleAndArguments(t *testing.T) {
	got := Localize("zh-CN", "newEmailFromBody", map[string]string{"sender": "艾达"})
	want := "来自 艾达"
	if got != want {
		t.Fatalf("Localize() = %q, want %q", got, want)
	}
}

func TestLocalizeFallsBackToEnglish(t *testing.T) {
	got := Localize("fr-FR", "newEmailTitle", nil)
	want := "New email"
	if got != want {
		t.Fatalf("Localize() = %q, want %q", got, want)
	}
}

func TestLocalizeUnknownKeyReturnsKey(t *testing.T) {
	if got := Localize("en", "missingKey", nil); got != "missingKey" {
		t.Fatalf("Localize() = %q, want missingKey", got)
	}
}

func TestLocalizeEmptyLanguageUsesEnglish(t *testing.T) {
	if got := Localize("", "newEmailNoSubject", nil); got != "(No subject)" {
		t.Fatalf("Localize() = %q, want (No subject)", got)
	}
}
