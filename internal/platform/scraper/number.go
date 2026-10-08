package scraper

import (
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ParseCount 将平台展示的计数文本解析为整数, 适配常见的千位/万位及以上数量级的
// 缩写与分隔符写法。支持:
//
//	纯数字:       "123"、"1,234"、"1 234"、欧洲点号千分位 "1.314"
//	英文缩写:     "1.04K"→1040、"1.2M"→1200000、"3B"→3000000000
//	英文单词:     "1.04 thousand"→1040、"1.2 million"→1200000、"3 billion"→3000000000
//	中文计数:     "1.2万"→12000、"2.5亿"→250000000、"3千"→3000
//	带单位/前后缀: "1.5K views"→1500、"12.5万 位订阅者"→125000
//
// 无法解析时返回 0。
func ParseCount(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	// 归一化: 统一小写, 并将所有空白(含不间断空格)折叠为单个空格。
	s = strings.ToLower(s)
	s = strings.Join(strings.Fields(s), " ")
	// 数字之间的空格视为千分位分隔符: "1 234" → "1234"(字母前空格保留)。
	s = collapseSpaceThousands(s)

	// 定位首个数字, 提取数字字面量(数字/小数点/逗号)。
	start := strings.IndexFunc(s, func(r rune) bool { return r >= '0' && r <= '9' })
	if start < 0 {
		return 0
	}
	i := start
	for i < len(s) && ((s[i] >= '0' && s[i] <= '9') || s[i] == '.' || s[i] == ',') {
		i++
	}
	num := s[start:i]
	if num == "" {
		return 0
	}

	// 识别数量级后缀(紧邻的单字后缀, 或后续的英文单词)。
	multiplier, hasSuffix := detectMultiplier(s[i:])

	// 归一化千分位/小数点分隔符。
	num = normalizeCountSeparators(num, hasSuffix)

	val, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0
	}
	// 用 Round 而非直接 int() 截断, 避免 "1.005K" 这类浮点误差导致少 1。
	return int(math.Round(val * multiplier))
}

// collapseSpaceThousands 移除"数字 数字"之间的空格(千分位分隔符), 其余空格保留。
// 例如 "1 234"→"1234", 而 "1.04 thousand" 中的空格(后跟字母)保持不变。
func collapseSpaceThousands(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' && i > 0 && i+1 < len(s) &&
			s[i-1] >= '0' && s[i-1] <= '9' && s[i+1] >= '0' && s[i+1] <= '9' {
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// detectMultiplier 识别紧随数字之后的数量级后缀, 返回倍率及是否存在后缀。
// 支持英文单词(hundred/thousand/million/billion/trillion 及复数)、
// 单字母缩写(k/m/b)以及中文单字(千/万/萬/亿/億)。
func detectMultiplier(rest string) (float64, bool) {
	rest = strings.TrimLeft(rest, " ")
	if rest == "" {
		return 1.0, false
	}

	// 先尝试匹配英文单词(优先于单字母, 避免 "thousand" 被误判为 't')。
	if word := leadingAlpha(rest); word != "" {
		switch strings.TrimSuffix(word, "s") {
		case "hundred":
			return 100.0, true
		case "thousand":
			return 1e3, true
		case "million":
			return 1e6, true
		case "billion":
			return 1e9, true
		case "trillion":
			return 1e12, true
		}
		// 单个字母缩写: k/m/b。
		if len(word) == 1 {
			switch word {
			case "k":
				return 1e3, true
			case "m":
				return 1e6, true
			case "b":
				return 1e9, true
			}
		}
		// 其他字母开头词(views/subscribers/members 等)不是数量级。
		return 1.0, false
	}

	// 中文单字后缀: 千/万/萬/亿/億。
	r, _ := utf8.DecodeRuneInString(rest)
	switch r {
	case '千':
		return 1e3, true
	case '万', '萬':
		return 1e4, true
	case '亿', '億':
		return 1e8, true
	}
	return 1.0, false
}

// leadingAlpha 返回字符串开头连续 ASCII 字母组成的子串。
func leadingAlpha(s string) string {
	i := 0
	for i < len(s) {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			i++
		} else {
			break
		}
	}
	return s[:i]
}

// normalizeCountSeparators 将数字部分中的千分位/小数点分隔符归一化为标准小数形式。
func normalizeCountSeparators(num string, hasSuffix bool) string {
	hasComma := strings.Contains(num, ",")
	hasDot := strings.Contains(num, ".")
	switch {
	case hasComma && hasDot:
		// 同时含逗号与句号: 后出现者视为小数点, 先出现者视为千分位。
		if strings.LastIndex(num, ",") > strings.LastIndex(num, ".") {
			// 逗号为小数点(欧洲), 句号为千分位。
			num = strings.ReplaceAll(num, ".", "")
			num = strings.ReplaceAll(num, ",", ".")
		} else {
			// 句号为小数点, 逗号为千分位。
			num = strings.ReplaceAll(num, ",", "")
		}
	case hasComma:
		if hasSuffix {
			// 欧洲小数: "1,2K" → 1.2
			num = strings.ReplaceAll(num, ",", ".")
		} else {
			// 英语千分位: "1,234" → 1234
			num = strings.ReplaceAll(num, ",", "")
		}
	case hasDot:
		if !hasSuffix {
			// 无后缀时需区分小数点("1.5")与欧洲千分位("1.314")。
			num = collapseDotThousands(num)
		}
		// 有后缀时句号即小数点: "1.04K" → 1.04, 保持原样。
	}
	return num
}

// collapseDotThousands 将"句号后恰好3位数字且其后非数字"的句号视为千分位并去除,
// 用于处理越南/欧洲格式 "1.314"→"1314"、"1.314.567"→"1314567", 同时保留 "1.5"、
// "1.04" 这类小数点。
func collapseDotThousands(s string) string {
	runes := []rune(s)
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(runes); i++ {
		if runes[i] == '.' && i+3 < len(runes) &&
			runes[i+1] >= '0' && runes[i+1] <= '9' &&
			runes[i+2] >= '0' && runes[i+2] <= '9' &&
			runes[i+3] >= '0' && runes[i+3] <= '9' &&
			(i+4 >= len(runes) || runes[i+4] < '0' || runes[i+4] > '9') {
			// 千分位, 去除句号。
			continue
		}
		b.WriteRune(runes[i])
	}
	return b.String()
}
