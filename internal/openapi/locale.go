package openapi

import (
	"os"
	"regexp"
	"strings"
	"unicode/utf8"
)

// localeEnvKeys 是探测默认语言时读取的环境变量，按优先级从高到低。
var localeEnvKeys = []string{"LC_ALL", "LC_MESSAGES", "LANG"}

// langOperatorPattern 匹配 query 里已有的 lang: 操作符（含 -lang: 与大小写变体）。
var langOperatorPattern = regexp.MustCompile(`(?i)(^|\s)-?lang:`)

// DetectLocale 从运行环境推断主语言码（两位小写字母），无法确定时返回空串。
//
// 只读取 LC_ALL、LC_MESSAGES、LANG 三个环境变量；原生 Windows 的 cmd/PowerShell
// 通常不设置它们，此时返回空串，需要显式配置 default_lang 或由服务管理器注入 LANG。
func DetectLocale() string {
	for _, key := range localeEnvKeys {
		value, ok := os.LookupEnv(key)
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		// POSIX 覆盖语义：LC_ALL 为 C/POSIX 时表示明确不要本地化，不再回退。
		if isNeutralLocale(value) {
			return ""
		}
		if code := parseLocale(value); code != "" {
			return code
		}
	}
	return ""
}

// parseLocale 从 POSIX 风格 locale 中提取两位主语言码，无法识别时返回空串。
// 支持 zh_CN.UTF-8、zh-CN、zh、sr_RS@latin 等写法。
func parseLocale(raw string) string {
	s := trimLocale(strings.TrimSpace(raw))
	if i := strings.IndexAny(s, "_-"); i >= 0 {
		s = s[:i]
	}
	if len(s) != 2 {
		return ""
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return ""
		}
	}
	return strings.ToLower(s)
}

// trimLocale 去掉 locale 中的编码（.UTF-8）与修饰（@latin）部分。
func trimLocale(s string) string {
	if i := strings.IndexAny(s, ".@"); i >= 0 {
		s = s[:i]
	}
	return s
}

// isNeutralLocale 判断 locale 是否表示“不做本地化”（C、POSIX）。
func isNeutralLocale(raw string) bool {
	base := strings.ToLower(trimLocale(strings.TrimSpace(raw)))
	return base == "c" || base == "posix"
}

// LangDecision 描述默认语言补全的结果，供调用方记录日志。
type LangDecision string

const (
	langDisabled LangDecision = "disabled"
	langPresent  LangDecision = "present"
	langAppended LangDecision = "appended"
	langTooLong  LangDecision = "too-long"
)

// applyDefaultLang 在 query 未显式包含 lang: 操作符时追加 " lang:<code>"。
// lang 为空表示功能关闭；补全后超过 128 个字符时保持原 query 不变。
func applyDefaultLang(query, lang string) (string, LangDecision) {
	if lang == "" {
		return query, langDisabled
	}
	if langOperatorPattern.MatchString(query) {
		return query, langPresent
	}
	augmented := query + " lang:" + lang
	if utf8.RuneCountInString(augmented) > maxQueryRunes {
		return query, langTooLong
	}
	return augmented, langAppended
}
