package openapi

import (
	"strings"
	"testing"
)

func TestParseLocale(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"", ""},
		{"C", ""},
		{"POSIX", ""},
		{"C.UTF-8", ""},
		{"zh_CN.UTF-8", "zh"},
		{"zh-CN", "zh"},
		{"zh", "zh"},
		{"ZH_CN", "zh"},
		{"sr_RS@latin", "sr"},
		{"en_US.utf8", "en"},
		{"fil_PH", ""},
		{"Chinese (Simplified)_China.936", ""},
		{"  zh_CN.UTF-8  ", "zh"},
	}
	for _, tc := range cases {
		if got := parseLocale(tc.raw); got != tc.want {
			t.Errorf("parseLocale(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestDetectLocale(t *testing.T) {
	cases := []struct {
		name              string
		lcAll, lcMessages string
		lang, want        string
	}{
		{"LC_ALL 优先", "zh_CN.UTF-8", "en_US.UTF-8", "fr_FR.UTF-8", "zh"},
		{"回退 LC_MESSAGES", "", "en_US.UTF-8", "fr_FR.UTF-8", "en"},
		{"回退 LANG", "", "", "fr_FR.UTF-8", "fr"},
		{"LC_ALL=C 阻断回退", "C", "en_US.UTF-8", "fr_FR.UTF-8", ""},
		{"POSIX 视为无语言", "POSIX", "", "zh_CN", ""},
		{"无法解析则继续回退", "fil_PH", "en_US.UTF-8", "", "en"},
		{"全部为空", "", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LC_ALL", tc.lcAll)
			t.Setenv("LC_MESSAGES", tc.lcMessages)
			t.Setenv("LANG", tc.lang)
			if got := DetectLocale(); got != tc.want {
				t.Fatalf("DetectLocale() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestApplyDefaultLang(t *testing.T) {
	long := strings.Repeat("a", maxQueryRunes)
	cases := []struct {
		name     string
		query    string
		lang     string
		want     string
		decision LangDecision
	}{
		{"关闭", "golang", "", "golang", langDisabled},
		{"补全", "golang", "zh", "golang lang:zh", langAppended},
		{"已有 lang", "golang lang:en", "zh", "golang lang:en", langPresent},
		{"已有大写 LANG", "golang LANG:en", "zh", "golang LANG:en", langPresent},
		{"已有否定 lang", "golang -lang:en", "zh", "golang -lang:en", langPresent},
		{"短语内的 lang", `foo "bar lang:en"`, "zh", `foo "bar lang:en"`, langPresent},
		{"补全后超长", long, "zh", long, langTooLong},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, decision := applyDefaultLang(tc.query, tc.lang)
			if got != tc.want || decision != tc.decision {
				t.Fatalf("applyDefaultLang(%q, %q) = (%q, %q), want (%q, %q)",
					tc.query, tc.lang, got, decision, tc.want, tc.decision)
			}
		})
	}
}
