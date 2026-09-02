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
