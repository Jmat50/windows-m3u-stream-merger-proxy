package sourceproc

import (
	"sync"
	"testing"
)

func TestParseChannelSourceRules(t *testing.T) {
	rules := parseChannelSourceRules([]string{
		`^ESPN$|1,2`,
		`invalid`,
		`^FOX$|`,
		`^CNN$| 3 , 4 `,
	})

	if len(rules) != 2 {
		t.Fatalf("expected 2 valid rules, got %d", len(rules))
	}

	if !rules[0].titleRegex.MatchString("ESPN") {
		t.Fatalf("first rule regex does not match expected title")
	}
	if _, ok := rules[0].sourceIndexes["1"]; !ok {
		t.Fatalf("expected source 1 in first rule")
	}
	if _, ok := rules[0].sourceIndexes["2"]; !ok {
		t.Fatalf("expected source 2 in first rule")
	}

	if !rules[1].titleRegex.MatchString("CNN") {
		t.Fatalf("second rule regex does not match expected title")
	}
	if _, ok := rules[1].sourceIndexes["3"]; !ok {
		t.Fatalf("expected source 3 in second rule")
	}
	if _, ok := rules[1].sourceIndexes["4"]; !ok {
		t.Fatalf("expected source 4 in second rule")
	}
}

func TestMatchChannelSourceRule(t *testing.T) {
	originalRules := channelRules
	t.Cleanup(func() {
		channelRules = originalRules
	})

	channelRules = parseChannelSourceRules([]string{
		`^ESPN$|1,2`,
	})

	if !matchChannelSourceRule(&StreamInfo{Title: "ESPN", SourceM3U: "1"}) {
		t.Fatalf("expected ESPN from source 1 to be allowed")
	}
	if matchChannelSourceRule(&StreamInfo{Title: "ESPN", SourceM3U: "3"}) {
		t.Fatalf("expected ESPN from source 3 to be blocked")
	}
	if !matchChannelSourceRule(&StreamInfo{Title: "CNN", SourceM3U: "9"}) {
		t.Fatalf("expected unmatched titles to pass")
	}
}

func TestMatchChannelSourceRuleMultipleMatches(t *testing.T) {
	originalRules := channelRules
	t.Cleanup(func() {
		channelRules = originalRules
	})

	channelRules = parseChannelSourceRules([]string{
		`^ESPN$|1`,
		`^ESPN$|3`,
	})

	if !matchChannelSourceRule(&StreamInfo{Title: "ESPN", SourceM3U: "3"}) {
		t.Fatalf("expected matching any allowed rule to pass")
	}
}

func TestParseChannelMergeRules(t *testing.T) {
	rules := parseChannelMergeRules([]string{
		"FOX Sports|FOX Sports HD",
		"invalid",
		"ESPN|",
		"ESPN 2| ESPN 2 US ",
		"ABC|ABC",
	})

	if len(rules) != 2 {
		t.Fatalf("expected 2 valid merge rules, got %d", len(rules))
	}

	if got := rules[normalizeChannelMergeKey("FOX Sports")]; got != "FOX Sports HD" {
		t.Fatalf("unexpected FOX merge target: %q", got)
	}
	if got := rules[normalizeChannelMergeKey("ESPN 2")]; got != "ESPN 2 US" {
		t.Fatalf("unexpected ESPN 2 merge target: %q", got)
	}
}

func TestApplyChannelMergeRule(t *testing.T) {
	originalOnce := filterOnce
	originalMerges := channelMerges
	t.Cleanup(func() {
		filterOnce = originalOnce
		channelMerges = originalMerges
	})

	filterOnce = sync.Once{}
	filterOnce.Do(func() {})
	channelMerges = map[string]string{
		normalizeChannelMergeKey("espn2"):       "ESPN 2 US",
		normalizeChannelMergeKey("espn 2 us"):   "ESPN",
		normalizeChannelMergeKey("news alpha"):  "News Global",
		normalizeChannelMergeKey("news global"): "News Global HD",
	}

	if got := applyChannelMergeRule("ESPN2"); got != "ESPN" {
		t.Fatalf("expected chained merge result ESPN, got %q", got)
	}
	if got := applyChannelMergeRule("news alpha"); got != "News Global HD" {
		t.Fatalf("expected chained merge result News Global HD, got %q", got)
	}
	if got := applyChannelMergeRule("Unmapped"); got != "Unmapped" {
		t.Fatalf("expected unmapped title to remain unchanged, got %q", got)
	}
}

func TestApplyChannelMergeRuleCycle(t *testing.T) {
	originalOnce := filterOnce
	originalMerges := channelMerges
	t.Cleanup(func() {
		filterOnce = originalOnce
		channelMerges = originalMerges
	})

	filterOnce = sync.Once{}
	filterOnce.Do(func() {})
	channelMerges = map[string]string{
		normalizeChannelMergeKey("alpha"): "Bravo",
		normalizeChannelMergeKey("bravo"): "Alpha",
	}

	if got := applyChannelMergeRule("Alpha"); got != "Alpha" {
		t.Fatalf("expected cyclic rule to preserve original title, got %q", got)
	}
}

func TestApplyChannelMergeRuleWhitespaceNormalization(t *testing.T) {
	originalOnce := filterOnce
	originalMerges := channelMerges
	t.Cleanup(func() {
		filterOnce = originalOnce
		channelMerges = originalMerges
	})

	filterOnce = sync.Once{}
	filterOnce.Do(func() {})
	channelMerges = map[string]string{
		normalizeChannelMergeKey("cartoon network usa eastern feed"): "Cartoon Network",
	}

	if got := applyChannelMergeRule("  Cartoon   Network   USA  Eastern Feed "); got != "Cartoon Network" {
		t.Fatalf("expected normalized whitespace merge result Cartoon Network, got %q", got)
	}
}

func TestApplyChannelMergeRulePunctuationNormalization(t *testing.T) {
	originalOnce := filterOnce
	originalMerges := channelMerges
	t.Cleanup(func() {
		filterOnce = originalOnce
		channelMerges = originalMerges
	})

	filterOnce = sync.Once{}
	filterOnce.Do(func() {})
	channelMerges = map[string]string{
		normalizeChannelMergeKey("Vice TV"): "VICE",
	}

	if got := applyChannelMergeRule("Vice-TV"); got != "VICE" {
		t.Fatalf("expected punctuation-normalized merge result VICE, got %q", got)
	}
	if got := applyChannelMergeRule("VICE.TV"); got != "VICE" {
		t.Fatalf("expected punctuation-normalized merge result VICE, got %q", got)
	}
}
