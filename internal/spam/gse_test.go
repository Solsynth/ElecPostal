package spam

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// TestGseSegmenterCutsChinese exercises the real embedded dictionary: the cut
// must preserve every character of a pure-CJK input and produce multi-word
// output (a single echo of the whole run would be the bug this exists to
// prevent).
func TestGseSegmenterCutsChinese(t *testing.T) {
	segmenter, err := NewChineseSegmenter()
	if err != nil {
		t.Fatalf("NewChineseSegmenter() error = %v", err)
	}

	for _, input := range []string{"免费领取比特币", "恭喜中奖请点击链接", "银行转账高额回报"} {
		words := segmenter.Cut(input)
		if len(words) < 2 {
			t.Fatalf("Cut(%q) = %v, want at least two words", input, words)
		}
		if joined := strings.Join(words, ""); joined != input {
			t.Fatalf("Cut(%q) = %v, characters lost (joined %q)", input, words, joined)
		}
		for _, word := range words {
			if word == "" {
				t.Fatalf("Cut(%q) returned an empty word: %v", input, words)
			}
		}
		t.Logf("Cut(%q) = %v", input, words)
	}
}

// TestGseSegmenterDeterministicUnderConcurrency: the adapter serializes cuts
// because gse's cutter carries state; this fails under -race if that guard is
// removed or becomes insufficient.
func TestGseSegmenterDeterministicUnderConcurrency(t *testing.T) {
	segmenter, err := NewChineseSegmenter()
	if err != nil {
		t.Fatalf("NewChineseSegmenter() error = %v", err)
	}
	const input = "免费领取限时优惠恭喜中奖银行转账高额回报投资理财低价代购秒杀抢购"
	want := fmt.Sprint(segmenter.Cut(input))

	var wg sync.WaitGroup
	errs := make(chan string, 32)
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				if got := fmt.Sprint(segmenter.Cut(input)); got != want {
					select {
					case errs <- got:
					default:
					}
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for got := range errs {
		t.Fatalf("concurrent cut differs: %s, want %s", got, want)
	}
}

// CJK corpora. Fragments are disjoint across classes and every message is
// assembled from them by seed, so training and held-out messages share
// vocabulary without being identical.
var (
	cjkSpamFragments = []string{"免费领取", "限时优惠", "点击链接", "恭喜中奖", "银行转账", "高额回报", "投资理财", "低价代购", "秒杀抢购", "扫码加群"}
	cjkHamFragments  = []string{"会议纪要", "项目进度", "本周报告", "需求评审", "部署上线", "文档更新", "排期确认", "数据同步", "预算审批", "季度总结"}
)

func cjkSpamMessage(seed int) Input {
	words := pickWords(cjkSpamFragments, seed, 6)
	subject := strings.Join(words[:2], "") + "!!!!!"
	body := strings.Join(words[2:], "，") + "，请立即点击"
	for i := 0; i < 6; i++ {
		body += fmt.Sprintf(" https://spam.example/offer%d", i)
	}
	return Input{
		FromAddress: fmt.Sprintf("sender%d@spam%d.example", seed, seed),
		Subject:     subject,
		Body:        body,
		ContentType: "text/plain",
	}
}

func cjkHamMessage(seed int) Input {
	words := pickWords(cjkHamFragments, seed, 6)
	return Input{
		FromAddress: fmt.Sprintf("alice%d@corp%d.example", seed, seed),
		Subject:     strings.Join(words[:2], ""),
		Body:        strings.Join(words[2:], "，") + "，谢谢。",
		ContentType: "text/plain",
	}
}

// TestBayesCorpusCJK runs the corpus test for Chinese mail in both
// tokenization modes: without a segmenter (character bigrams) and with the
// real dictionary segmenter. Both must route correctly; the logged scores show
// how much sharper one is.
func TestBayesCorpusCJK(t *testing.T) {
	segmenter, err := NewChineseSegmenter()
	if err != nil {
		t.Fatalf("NewChineseSegmenter() error = %v", err)
	}

	for _, tc := range []struct {
		name      string
		segmenter Segmenter
	}{
		{"bigrams", nil},
		{"segmenter", segmenter},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := NewService(Config{BayesEnabled: true, MinLearns: 10, MinTokens: 11, Segmenter: tc.segmenter}, newMemStore())
			ctx := context.Background()
			for i := 0; i < 30; i++ {
				if err := svc.Learn(ctx, cjkSpamMessage(i), "spam"); err != nil {
					t.Fatal(err)
				}
				if err := svc.Learn(ctx, cjkHamMessage(i), "ham"); err != nil {
					t.Fatal(err)
				}
			}

			spamLow, hamHigh := 0.0, 0.0
			for i := 0; i < 5; i++ {
				result := svc.Score(ctx, cjkSpamMessage(1000+i))
				t.Logf("held-out spam %d: score=%.2f symbols=%v", i, result.Score, result.Symbols)
				if result.Score < 5.0 {
					t.Errorf("held-out spam %d: score %.2f, want >= 5.0", i, result.Score)
				}
				if !hasSymbol(result.Symbols, SymbolBayesSpam) {
					t.Errorf("held-out spam %d: BAYES_SPAM missing: %v", i, result.Symbols)
				}
				if i == 0 || result.Score < spamLow {
					spamLow = result.Score
				}

				result = svc.Score(ctx, cjkHamMessage(1000+i))
				t.Logf("held-out ham %d: score=%.2f symbols=%v", i, result.Score, result.Symbols)
				if result.Score > 0.5 {
					t.Errorf("held-out ham %d: score %.2f, want <= 0.5", i, result.Score)
				}
				if result.Score > hamHigh {
					hamHigh = result.Score
				}
			}
			t.Logf("%s: worst spam %.2f, worst ham %.2f", tc.name, spamLow, hamHigh)
		})
	}
}

// TestGseSegmenterTokenCoverage: the cap must still be respected with real
// segmentation, and a long Chinese body must yield features (not one token).
func TestGseSegmenterTokenCoverage(t *testing.T) {
	segmenter, err := NewChineseSegmenter()
	if err != nil {
		t.Fatalf("NewChineseSegmenter() error = %v", err)
	}
	body := strings.Repeat("免费领取限时优惠恭喜中奖银行转账高额回报投资理财", 40)
	toks := tokenizeTokens(Input{Body: body}, segmenter)
	if len(toks) < 20 {
		t.Fatalf("token count = %d, want a real feature set", len(toks))
	}
	if len(toks) > maxTokens {
		t.Fatalf("token count %d exceeds cap %d", len(toks), maxTokens)
	}
	for _, tk := range toks {
		if tk.fw != 0.5 && tk.fw != 1.0 {
			t.Fatalf("unexpected feature weight %v", tk.fw)
		}
	}
	if runs := utf8.RuneCountInString(body); runs < maxTokens {
		t.Fatalf("test setup: body too short (%d runes)", runs)
	}
}
