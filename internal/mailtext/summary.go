package mailtext

import (
	"strings"
	"unicode"

	"golang.org/x/net/html"
)

func Summary(body, contentType string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	var text summaryBuilder
	text.limit = maxRunes
	if strings.EqualFold(strings.TrimSpace(contentType), "text/html") {
		htmlText(body, &text)
	} else {
		text.write(body)
	}
	return strings.TrimSpace(text.value.String())
}

type summaryBuilder struct {
	value     strings.Builder
	limit     int
	runes     int
	lastSpace bool
}

func (s *summaryBuilder) write(value string) {
	for _, r := range value {
		if unicode.IsSpace(r) {
			if s.runes > 0 && !s.lastSpace {
				s.value.WriteByte(' ')
				s.runes++
				s.lastSpace = true
			}
			continue
		}
		if s.runes >= s.limit {
			return
		}
		s.value.WriteRune(r)
		s.runes++
		s.lastSpace = false
	}
}

func (s *summaryBuilder) full() bool { return s.runes >= s.limit }

func htmlText(source string, text *summaryBuilder) {
	z := html.NewTokenizer(strings.NewReader(source))
	blocked := 0
	for !text.full() {
		tokenType := z.Next()
		switch tokenType {
		case html.ErrorToken:
			return
		case html.StartTagToken, html.SelfClosingTagToken:
			token := z.Token()
			if token.Data == "script" || token.Data == "style" || token.Data == "head" {
				blocked++
			}
			if blocked == 0 && isBlockTag(token.Data) {
				text.write(" ")
			}
		case html.EndTagToken:
			token := z.Token()
			if token.Data == "script" || token.Data == "style" || token.Data == "head" {
				if blocked > 0 {
					blocked--
				}
			}
			if blocked == 0 && isBlockTag(token.Data) {
				text.write(" ")
			}
		case html.TextToken:
			if blocked == 0 {
				text.write(string(z.Text()))
			}
		}
	}
}

func isBlockTag(tag string) bool {
	switch tag {
	case "br", "p", "div", "li", "tr", "h1", "h2", "h3":
		return true
	default:
		return false
	}
}
