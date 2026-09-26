// Package localization contains the notification copy shared by ElecPostal's
// outbound Ring messages. The keys and locale fallback match the DysonNetwork
// fleet's JSON localization service.
package localization

import "strings"

var messages = map[string]map[string]string{
	"en": {
		"newEmailTitle":         "New email",
		"newEmailFromBody":      "From {sender}",
		"newEmailNoSubject":     "(No subject)",
		"newEmailUnknownSender": "New sender",
		"verificationCodeTitle": "Verification code",
		"securityAlertTitle":    "Security alert",
		"actionRequiredTitle":   "Action required",
	},
	"zh-hans": {
		"newEmailTitle":         "新邮件",
		"newEmailFromBody":      "来自 {sender}",
		"newEmailNoSubject":     "（无主题）",
		"newEmailUnknownSender": "新发件人",
		"verificationCodeTitle": "验证码",
		"securityAlertTitle":    "安全提醒",
		"actionRequiredTitle":   "需要处理",
	},
}

// Localize returns the requested message with named placeholders replaced.
// Languages use the same normalization and English fallback as the fleet:
// zh-* maps to zh-hans, en-* maps to en, and unsupported languages use en.
func Localize(language, key string, args map[string]string) string {
	locale := normalizeLocale(language)
	text, ok := messages[locale][key]
	if !ok {
		text, ok = messages["en"][key]
	}
	if !ok {
		text = key
	}
	for name, value := range args {
		text = strings.ReplaceAll(text, "{"+name+"}", value)
	}
	return text
}

func normalizeLocale(language string) string {
	language = strings.ToLower(strings.TrimSpace(language))
	switch {
	case strings.HasPrefix(language, "zh"):
		return "zh-hans"
	case strings.HasPrefix(language, "en"):
		return "en"
	default:
		return "en"
	}
}
