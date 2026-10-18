package main

import "testing"

// v2.1.0：版本提示必须按段做数字比较（字符串比较会把 2.0.10 判成小于 2.0.9）
func TestVerLess(t *testing.T) {
	cases := []struct {
		cur, latest string
		want        bool
	}{
		{"2.0.9", "2.1.0", true},   // 常规升级
		{"2.1.0", "2.0.9", false},  // 不降级
		{"2.0.9", "2.0.9", false},  // 相同
		{"2.0.9", "2.0.10", true},  // ★字符串比较会判错的一例
		{"2.0.10", "2.0.9", false}, // ★反向
		{"2.0", "2.0.1", true},     // 段数不齐
		{"2.0.1", "2.0", false},
		{"v2.0.9", "2.1.0", true}, // 带 v 前缀
		{"", "2.1.0", true},       // 本机版本未知
		{"2.1.0", "", false},      // 服务端未下发
		{"1.9.9", "2.0.0", true},
		{"10.0.0", "9.9.9", false}, // 两位数段
	}
	for _, c := range cases {
		if got := verLess(c.cur, c.latest); got != c.want {
			t.Errorf("verLess(%q,%q) = %v, want %v", c.cur, c.latest, got, c.want)
		}
	}
}
