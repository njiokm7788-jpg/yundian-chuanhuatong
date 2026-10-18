// 云点传话筒 · 教室端（Go 版）
//
// 与 Python 版（yundianchuanhuatong.py + webui.py + pywebview）协议一致：
//
//	wss://<your-domain>/api/device/ws?token=   实时推送（断开自动降级 HTTP 轮询）
//	本机 HTTP 服务：/ui/* 界面、/rpc/* 桥、/events 推送、/audio 音频
//
// 浏览器壳用 Edge --app（无地址栏），配置目录沿用 Python 版：
//
//	%APPDATA%\YundianChuanhuatong\{config.json,audio\device.log}
package main

import (
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	appName  = "云点传话筒"
	version  = "2.2.7"
	defBase  = "https://your-domain.example.com"
	uaString = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	defPort = 47632
)

// basePort 本机服务起始端口；YDC_PORT 可指定（测试/并行实例用，正式运行不设置）
func basePort() int {
	if s := os.Getenv("YDC_PORT"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 1024 && n < 65000 {
			return n
		}
	}
	return defPort
}

var (
	confDir  string
	confFile string
	cacheDir string
	logFile  string
	exeDir   string
)

func initPaths() {
	home := os.Getenv("YDC_HOME")
	if home == "" {
		home = os.Getenv("APPDATA")
	}
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	confDir = filepath.Join(home, "YundianChuanhuatong")
	confFile = filepath.Join(confDir, "config.json")
	cacheDir = filepath.Join(confDir, "audio")
	logFile = filepath.Join(confDir, "device.log")
	if exe, err := os.Executable(); err == nil {
		exeDir = filepath.Dir(exe)
	} else {
		exeDir, _ = os.Getwd()
	}
	migrateLegacyHome(home)
}

// migrateLegacyHome 把旧名（Chuanhuatong）时代的配置目录内容**复制**到新目录（不删旧目录）：
// 老用户直接换新版 exe，绑定关系与设置继续有效，不用重新绑定。
func migrateLegacyHome(home string) {
	old := filepath.Join(home, "Chuanhuatong")
	if old == confDir {
		return
	}
	if _, err := os.Stat(filepath.Join(old, "config.json")); err != nil {
		return // 没有旧配置，无需迁移
	}
	if _, err := os.Stat(confFile); err == nil {
		return // 新目录已有配置，以新的为准
	}
	_ = os.MkdirAll(cacheDir, 0o755)
	for _, n := range []string{"config.json", "device.log"} {
		if b, err := os.ReadFile(filepath.Join(old, n)); err == nil {
			_ = os.WriteFile(filepath.Join(confDir, n), b, 0o644)
		}
	}
	if es, err := os.ReadDir(filepath.Join(old, "audio")); err == nil {
		for _, e := range es {
			if e.IsDir() {
				continue
			}
			if b, err := os.ReadFile(filepath.Join(old, "audio", e.Name())); err == nil {
				_ = os.WriteFile(filepath.Join(cacheDir, e.Name()), b, 0o644)
			}
		}
	}
	logf("已迁移旧配置目录（%s → %s）：绑定关系与设置继续有效", old, confDir)
}

func ensureDirs() {
	for _, d := range []string{confDir, cacheDir} {
		_ = os.MkdirAll(d, 0o755)
	}
}

var logMu sync.Mutex

// logf 日志格式与 Python 版一致，方便老用户对照排查
func logf(format string, a ...any) {
	line := fmt.Sprintf("[%s] %s", time.Now().Format("01-02 15:04:05"), fmt.Sprintf(format, a...))
	logMu.Lock()
	defer logMu.Unlock()
	fmt.Println(line)
	if fi, err := os.Stat(logFile); err == nil && fi.Size() > 512*1024 {
		_ = os.Remove(logFile)
	}
	if f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		_, _ = f.WriteString(line + "\n")
		_ = f.Close()
	}
}

// ---------------------------------------------------------------- 配置

var cfgMu sync.Mutex

func loadCfg() map[string]any {
	m := map[string]any{}
	if raw, err := os.ReadFile(confFile); err == nil {
		_ = json.Unmarshal(raw, &m)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m
}

func saveConfig(m map[string]any) error {
	cfgMu.Lock()
	raw, err := json.MarshalIndent(m, "", "  ")
	cfgMu.Unlock()
	if err != nil {
		return err
	}
	ensureDirs()
	tmp := confFile + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, confFile)
}

var saveTimer *time.Timer

func saveConfigDebounced(m map[string]any) {
	cfgMu.Lock()
	if saveTimer != nil {
		saveTimer.Stop()
	}
	saveTimer = time.AfterFunc(2*time.Second, func() { _ = saveConfig(m) })
	cfgMu.Unlock()
}

func cfgInt(m map[string]any, key string, def int) int {
	if m == nil {
		return def
	}
	if v, ok := m[key]; ok && v != nil {
		return atoiAny(v, def)
	}
	return def
}

func baseOf(m map[string]any) string {
	if s := os.Getenv("YDC_BASE"); s != "" {
		return s
	}
	if s := cfgStr(m, "base"); s != "" {
		return s
	}
	return defBase
}

// ---------------------------------------------------------------- 小工具

func textHash(s string) string {
	h := md5.Sum([]byte(strings.TrimSpace(s)))
	return hex.EncodeToString(h[:8])
}

func localHostName() string {
	if s := os.Getenv("COMPUTERNAME"); s != "" {
		return s
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "PC"
}

func nowMs() int64 { return time.Now().UnixNano() / int64(time.Millisecond) }

func atoiAny(v any, def int) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	case string:
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(t), "%d", &n); err == nil {
			return n
		}
	}
	return def
}

// verParts 把 "2.0.9" 切成 [2,0,9]（非数字段按 0 计）
func verParts(s string) []int {
	fields := strings.FieldsFunc(strings.TrimSpace(s), func(r rune) bool {
		return r == '.' || r == 'v' || r == 'V'
	})
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		n, _ := strconv.Atoi(strings.TrimSpace(f))
		out = append(out, n)
	}
	return out
}

// verLess 按段做数字比较：cur 是否比 latest 旧。
// 必须按段比较——纯字符串比较会把 "2.0.10" 判成小于 "2.0.9"。
func verLess(cur, latest string) bool {
	pa, pb := verParts(cur), verParts(latest)
	n := len(pa)
	if len(pb) > n {
		n = len(pb)
	}
	for i := 0; i < n; i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			return x < y
		}
	}
	return false
}

// wavDuration 读 WAV 头算时长（秒）
func wavDuration(b []byte) float64 {
	if len(b) < 44 || string(b[0:4]) != "RIFF" {
		return 0
	}
	var byteRate, dataSize uint32
	pos := 12
	for pos+8 <= len(b) {
		id := string(b[pos : pos+4])
		sz := binary.LittleEndian.Uint32(b[pos+4 : pos+8])
		body := pos + 8
		switch id {
		case "fmt ":
			if body+16 <= len(b) {
				byteRate = binary.LittleEndian.Uint32(b[body+8 : body+12])
			}
		case "data":
			dataSize = sz
			if int(body)+int(sz) > len(b) {
				dataSize = uint32(len(b) - body)
			}
		}
		if id == "data" {
			break
		}
		pos = body + int(sz) + int(sz%2)
	}
	if byteRate == 0 {
		return 0
	}
	return float64(dataSize) / float64(byteRate)
}

// ---------------------------------------------------------------- 入口

func main() {
	initPaths()
	ensureDirs()
	logf("%s 教室端(Go) v%s 启动", appName, version)

	// 已在运行 → 把已有窗口叫出来，本次直接退出
	if bringExistingToFront() {
		logf("检测到已在运行，显示已有窗口后退出")
		return
	}

	cfg := loadCfg()
	api := NewApi(baseOf(cfg))
	hub := NewHub()
	app := NewApp(api, hub, cfg)
	app.onQuit = func() { postShellQuit() } // 退出改为让主消息循环自然结束（窗口销毁由 doQuitApp 做）
	mainApp = app

	port, err := startServer(app, hub)
	if err != nil {
		logf("本地服务启动失败：%v", err)
		return
	}
	logf("本地服务端口 %d", port)
	srvPort = port
	app.winShow = showForApp // v2.2.0：窗口常驻，此处只做显示/隐藏切换
	startTray()              // 系统托盘：关掉界面后点托盘图标可重新打开

	noOpen := os.Getenv("YDC_NO_OPEN") == "1"
	forceSilent := false
	for _, a := range os.Args[1:] {
		switch a {
		case "--no-open":
			noOpen = true
		case "--silent":
			forceSilent = true
		}
	}

	// v2.0.8：只有开机自启/--silent（silentStart）才允许静默藏窗；手动双击必定显示窗口
	silentStart := noOpen || forceSilent
	app.silentStart = silentStart
	app.winVisible = windowVisible

	bound := cfgInt(cfg, "token", 0) != 0 || app.token != ""
	if !noOpen {
		if err := launchShell(port, bound && silentStart); err != nil {
			logf("打开界面失败：%v（可直接访问 http://127.0.0.1:%d/ui/）", err, port)
		}
	}

	if bound {
		if forceSilent {
			logf("静默启动（开机自启）")
		}
		app.StartLink()
	} else {
		setWinVisible(!noOpen)
	}

	// 托盘/控制台退出
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		logf("退出")
		_ = saveConfig(app.cfg)
		procDestroyWindow.Call(hwndMain)
	}()

	// v2.2.0：主消息循环跑在 main goroutine（WebView2 的 Dispatch/消息依赖它），
	// WM_QUIT（doQuitApp→postShellQuit / 销毁窗口 / Ctrl+C）退出
	runShellMessageLoop()
	logf("消息循环已退出，进程结束")
}
