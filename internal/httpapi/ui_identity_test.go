package httpapi

import "testing"

// 界面上的首字母标识：英文取大写，中日韩取首字，空值兜底。
func TestInitialOf(t *testing.T) {
	cases := map[string]string{
		"Cloud":     "C",
		"cloud":     "C",
		"演示服务":      "演",
		"  云盘  ":    "云",
		"":          "UC",
		"   ":       "UC",
		"1Password": "1",
	}
	for in, want := range cases {
		if got := initialOf(in); got != want {
			t.Errorf("initialOf(%q) = %q, want %q", in, got, want)
		}
	}
}
