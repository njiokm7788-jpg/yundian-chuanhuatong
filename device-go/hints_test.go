package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countLog 统计日志里某子串出现次数（applyHints 的切换会写日志）
func countLog(t *testing.T, sub string) int {
	t.Helper()
	b, err := os.ReadFile(logFile)
	if err != nil {
		return 0
	}
	return strings.Count(string(b), sub)
}

// v2.1.0：自检轮询（hintsLoop）与主链路（poll/checkState）会并发进 applyHints，
// base 的「比较+切换」必须原子：64 个 goroutine 同时打进来，只允许切一次。
// （把 baseMu 去掉本用例会挂在「切换日志 = 1 条」上）
func TestApplyHintsSwitchOnce(t *testing.T) {
	t.Setenv("YDC_HOME", t.TempDir())
	initPaths()
	ensureDirs()

	app := &App{api: NewApi("http://old.example"), cfg: map[string]any{}}
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			app.applyHints(map[string]any{"base": "http://new.example"})
		}()
	}
	wg.Wait()

	if got := app.api.Base(); got != "http://new.example" {
		t.Fatalf("Base() = %q, want http://new.example", got)
	}
	if got := cfgStr(app.cfg, "base"); got != "http://new.example" {
		t.Fatalf("cfg[base] = %q, want http://new.example", got)
	}
	if n := countLog(t, "服务端下发新域名"); n != 1 {
		t.Fatalf("切换日志 %d 条，want 1（base 切换不是原子的）", n)
	}
	// 幂等：同一个 base 再来一次不应有任何变化
	app.applyHints(map[string]any{"base": "http://new.example"})
	if n := countLog(t, "服务端下发新域名"); n != 1 {
		t.Fatalf("重复下发后又写了日志，共 %d 条", n)
	}
}

// YDC_BASE 优先级最高：环境变量存在时，下发不得覆盖 base
func TestApplyHintsRespectsBSTBase(t *testing.T) {
	t.Setenv("YDC_HOME", t.TempDir())
	t.Setenv("YDC_BASE", "http://forced.example")
	initPaths()
	ensureDirs()

	app := &App{api: NewApi("http://forced.example"), cfg: map[string]any{}}
	app.applyHints(map[string]any{"base": "http://delivered.example"})
	if got := app.api.Base(); got != "http://forced.example" {
		t.Fatalf("Base() = %q, want http://forced.example（YDC_BASE 必须赢）", got)
	}
	if got := cfgStr(app.cfg, "base"); got != "" {
		t.Fatalf("cfg[base] = %q, want 空（不应落盘下发值）", got)
	}
}

// 非法地址整条丢弃：不落盘也不改内存（服务端 [vars] 写错不能把整批设备写死）
func TestApplyHintsRejectsBadBase(t *testing.T) {
	t.Setenv("YDC_HOME", t.TempDir())
	initPaths()
	ensureDirs()

	for _, bad := range []string{"your-domain.example.com", "ftp://x/y", "javascript:alert(1)", "//host/x"} {
		app := &App{api: NewApi("http://old.example"), cfg: map[string]any{}}
		app.applyHints(map[string]any{"base": bad})
		if got := app.api.Base(); got != "http://old.example" {
			t.Fatalf("base=%q 被接受成 %q，want 保持 http://old.example", bad, got)
		}
		if got := cfgStr(app.cfg, "base"); got != "" {
			t.Fatalf("base=%q 被落盘成 %q，want 空", bad, got)
		}
	}
}

// ★401 之后循环必须能恢复：解绑/被踢 → 重新绑定后仍要收得到域名与版本下发。
// （旧实现 401 直接 return，而 hintsOnce 只放行一个 goroutine → 此用例会挂）
func TestHintsLoopSurvives401(t *testing.T) {
	t.Setenv("YDC_HOME", t.TempDir())
	initPaths()
	ensureDirs()

	var ok atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ok.Load() {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"latest":"9.9.9"}`)
	}))
	defer srv.Close()

	app := &App{api: NewApi(srv.URL), cfg: map[string]any{}, token: "t"}

	oldFirst, oldInterval := hintsFirstDelay, hintsInterval
	hintsFirstDelay, hintsInterval = 5*time.Millisecond, 5*time.Millisecond
	defer func() { hintsFirstDelay, hintsInterval = oldFirst, oldInterval }()

	go app.hintsLoop()
	time.Sleep(60 * time.Millisecond) // 先在 401 上转几圈
	ok.Store(true)                    // 等价于「重新绑定，凭证恢复」
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && app.latestStr() != "9.9.9" {
		time.Sleep(5 * time.Millisecond)
	}
	if app.latestStr() != "9.9.9" {
		t.Fatal("401 之后自检循环没有恢复：解绑重绑后收不到域名/版本下发")
	}
}
