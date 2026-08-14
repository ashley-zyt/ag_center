package scraper

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// SanitizeString 清理字符串中的非法UTF-8字节和控制字符,确保输出是合法的UTF-8
// chromedp从浏览器获取的数据有时包含孤立代理对、null字节或其他非法序列,
// 这些字符会导致Ruby端出现"incompatible character encodings: UTF-8 and ASCII-8BIT"错误
func SanitizeString(s string) string {
	if s == "" {
		return ""
	}
	// 1. 将非法UTF-8字节替换为U+FFFD
	s = strings.ToValidUTF8(s, "")
	// 2. 移除null字节(ASCII-8BIT最常见的来源)
	s = strings.ReplaceAll(s, "\x00", "")
	// 3. 清理控制字符(保留常用的\t\n\r)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\t' || r == '\n' || r == '\r' {
			b.WriteRune(r)
			continue
		}
		if unicode.IsControl(r) {
			continue
		}
		// 过滤孤立代理对(surrogate halves): U+D800-U+DFFF
		if r >= 0xD800 && r <= 0xDFFF {
			continue
		}
		// 过滤BOM和其他特殊不可见字符
		if r == 0xFEFF || r == 0xFFFE || r == 0xFFFF {
			continue
		}
		b.WriteRune(r)
	}
	result := b.String()
	// 4. 最终验证: 确保结果是合法UTF-8
	if !utf8.ValidString(result) {
		result = strings.ToValidUTF8(result, "")
	}
	return result
}

// SanitizePost 清理Post中所有字符串字段
func SanitizePost(p Post) Post {
	p.Title = SanitizeString(p.Title)
	p.Link = SanitizeString(p.Link)
	p.PublishTime = SanitizeString(p.PublishTime)
	return p
}

// SanitizeResult 清理FetchResult中所有字符串字段
func SanitizeResult(r FetchResult) FetchResult {
	for i, p := range r.Posts {
		r.Posts[i] = SanitizePost(p)
	}
	return r
}
