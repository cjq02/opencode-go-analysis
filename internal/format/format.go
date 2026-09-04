package format

import "fmt"

func Thousands(n int64) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func ThousandsF(f float64) string {
	s := fmt.Sprintf("%.2f", f)
	ip := len(s)
	for i, c := range s {
		if c == '.' {
			ip = i
			break
		}
	}
	for i := ip - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return "$" + s
}

func Per100MIn(cost float64, tokens int64) string {
	if tokens <= 0 {
		return "—"
	}
	return fmt.Sprintf("$%.2f", cost/float64(tokens)*1e8)
}

// CompactTokens 分享卡用的紧凑格式：>=1亿用“xx.xx亿”，>=1万用“xx.xx万”，否则千分位。
// 例如 11415638226 -> "114.16亿"，537987000 -> "5.38亿"，73267725 -> "7326.78万"。
func CompactTokens(n int64) string {
	if n >= 100_000_000 {
		return trim2(float64(n) / 1e8) + "亿"
	}
	if n >= 10_000 {
		return trim2(float64(n) / 1e4) + "万"
	}
	return Thousands(n)
}

func trim2(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	// 去掉多余的 0：114.10 -> 114.1，5.00 -> 5
	for len(s) > 0 && s[len(s)-1] == '0' {
		s = s[:len(s)-1]
	}
	if len(s) > 0 && s[len(s)-1] == '.' {
		s = s[:len(s)-1]
	}
	return s
}
