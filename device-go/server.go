package main

import (
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 界面内嵌进 exe（单文件分发，等价 Python 版 onefile 的 _MEIPASS/ui）
//
//go:embed ui
var uiEmbed embed.FS

// ---------------------------------------------------------------- 推送总线

// Hub 给页面推事件（SSE）。页面只有一个，但保留多订阅者以容忍刷新/多标签。
type Hub struct {
	mu   sync.Mutex
	n    int
	subs map[int]chan []byte
}

func NewHub() *Hub { return &Hub{subs: map[int]chan []byte{}} }

func (h *Hub) Add() (int, chan []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.n++
	id := h.n
	ch := make(chan []byte, 64)
	h.subs[id] = ch
	return id, ch
}

func (h *Hub) Del(id int) {
	h.mu.Lock()
	ch, ok := h.subs[id]
	if ok {
		delete(h.subs, id)
	}
	h.mu.Unlock()
	if ok {
		close(ch)
	}
}

func (h *Hub) Send(payload map[string]any) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	h.mu.Lock()
	chs := make([]chan []byte, 0, len(h.subs))
	for _, ch := range h.subs {
		chs = append(chs, ch)
	}
	h.mu.Unlock()
	for _, ch := range chs {
		select {
		case ch <- b:
		case <-time.After(2 * time.Second): // 页面卡住时不阻塞主流程
		}
	}
}

// ---------------------------------------------------------------- 本地服务

// externalUIDir 返回 exe 同目录/工作目录下真实存在的 ui/ 目录（改界面免编译）；
// 都没有时返回空串，改用内嵌副本。
func externalUIDir() string {
	cands := []string{filepath.Join(exeDir, "ui")}
	if wd, err := os.Getwd(); err == nil {
		cands = append(cands, filepath.Join(wd, "ui"))
	}
	for _, d := range cands {
		if _, err := os.Stat(filepath.Join(d, "index.html")); err == nil {
			return d
		}
	}
	return ""
}

var mimeByExt = map[string]string{
	".html": "text/html; charset=utf-8",
	".js":   "application/javascript; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".ico":  "image/x-icon",
	".png":  "image/png",
	".svg":  "image/svg+xml",
	".json": "application/json; charset=utf-8",
}

// serveUI 提供界面文件：优先外置 ui/，否则用 exe 内嵌副本（单文件分发）
func serveUI(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/ui/")
	if name == r.URL.Path { // /favicon.ico 等根路径资源
		name = strings.TrimPrefix(r.URL.Path, "/")
	}
	if name == "" {
		name = "index.html"
	}
	name = strings.TrimPrefix(filepath.ToSlash(filepath.Clean("/"+name)), "/")
	ext := strings.ToLower(filepath.Ext(name))
	ct, ok := mimeByExt[ext]
	if !ok {
		http.NotFound(w, r)
		return
	}
	var body []byte
	if dir := externalUIDir(); dir != "" {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		body = b
	} else {
		sub, err := fs.Sub(uiEmbed, "ui")
		if err != nil {
			http.Error(w, "ui missing", 500)
			return
		}
		b, err := fs.ReadFile(sub, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		body = b
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

func startServer(app *App, hub *Hub) (int, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			http.Redirect(w, r, "/ui/", http.StatusFound)
		case "/favicon.ico":
			serveUI(w, r) // 窗口/任务栏图标（Edge --app 会请求根路径图标）
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/ui/", serveUI)
	mux.HandleFunc("/rpc/", func(w http.ResponseWriter, r *http.Request) { handleRPC(app, w, r) })
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) { serveEvents(hub, w, r) })
	mux.HandleFunc("/audio", func(w http.ResponseWriter, r *http.Request) { serveAudio(app, w, r) })
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "ver": version, "app": appName})
	})

	var ln net.Listener
	var err error
	port := 0
	bp := basePort()
	for p := bp; p < bp+12; p++ {
		ln, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			port = p
			break
		}
	}
	if ln == nil {
		return 0, err
	}
	go func() { _ = http.Serve(ln, mux) }()
	return port, nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func serveEvents(hub *Hub, w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flush", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)
	fl.Flush()

	id, ch := hub.Add()
	logf("界面通道已连接(SSE #%d)", id)
	defer func() { hub.Del(id); logf("界面通道断开(SSE #%d)", id) }()
	ka := time.NewTicker(15 * time.Second)
	defer ka.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case b, ok := <-ch:
			if !ok {
				return
			}
			_, _ = w.Write([]byte("data: "))
			_, _ = w.Write(b)
			_, _ = w.Write([]byte("\n\n"))
			fl.Flush()
		case <-ka.C:
			_, _ = w.Write([]byte(": ping\n\n"))
			fl.Flush()
		}
	}
}

// ---------------------------------------------------------------- RPC 桥

type rpcReq struct {
	Args []any `json:"args"`
}

func argStr(args []any, i int) string {
	if i < len(args) && args[i] != nil {
		return fmt.Sprintf("%v", args[i])
	}
	return ""
}

func argInt(args []any, i int) int {
	if i < len(args) {
		return atoiAny(args[i], 0)
	}
	return 0
}

func argBool(args []any, i int, def bool) bool {
	if i < len(args) {
		return truthy(args[i])
	}
	return def
}

// handleRPC 把 JS 侧的 window.pywebview.api.* 调用转发到 Go
func handleRPC(app *App, w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/rpc/")
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var req rpcReq
	_ = json.Unmarshal(raw, &req)
	args := req.Args

	switch name {
	case "init":
		writeJSON(w, initResult(app))
	case "ui_ready":
		app.UiReady()
		writeJSON(w, map[string]any{"ok": true})
	case "set_config":
		patch := map[string]any{}
		if len(args) > 0 {
			if m, ok := args[0].(map[string]any); ok {
				patch = m
			}
		}
		writeJSON(w, map[string]any{"ok": true, "cfg": app.SetConfig(patch)})
	case "bind":
		res := app.Bind(argStr(args, 0), argStr(args, 1))
		writeJSON(w, res)
	case "unbind":
		writeJSON(w, app.Unbind())
	case "get_audio":
		mid := argInt(args, 0)
		data := app.store.Get(mid)
		logf("界面取音频 #%d 命中=%v", mid, data != nil)
		if data != nil {
			app.markPageGot(mid)
		}
		if data == nil {
			writeJSON(w, map[string]any{"ok": false})
			break
		}
		writeJSON(w, map[string]any{
			"ok": true, "b64": base64.StdEncoding.EncodeToString(data),
			"mime": "audio/wav", "ms": int(wavDuration(data) * 1000),
		})
	case "played":
		logf("界面回执 played #%d ok=%v", argInt(args, 0), argBool(args, 1, true))
		app.Played(argInt(args, 0), argBool(args, 1, true))
		writeJSON(w, map[string]any{"ok": true})
	case "ack":
		app.Ack(argInt(args, 0))
		writeJSON(w, map[string]any{"ok": true})
	case "view_done":
		app.ViewDone(argInt(args, 0))
		writeJSON(w, map[string]any{"ok": true})
	case "extend":
		app.ExtendFromUI(argInt(args, 0))
		writeJSON(w, map[string]any{"ok": true})
	case "min_window", "hide_window":
		setWinVisible(false)
		writeJSON(w, map[string]any{"ok": true})
	case "show_window":
		go showShellWindow("rpc-show")
		writeJSON(w, map[string]any{"ok": true})
	case "open_log":
		writeJSON(w, map[string]any{"ok": openFile(logFile)})
	case "open_dir":
		writeJSON(w, map[string]any{"ok": openFile(confDir)})
	case "open_url":
		writeJSON(w, map[string]any{"ok": openURL(argStr(args, 0))})
	case "set_autostart":
		on := argBool(args, 0, false)
		ok := setAutostart(on)
		writeJSON(w, map[string]any{"ok": ok, "on": autostartOn()})
	case "quit_app":
		writeJSON(w, map[string]any{"ok": true})
		go func() {
			time.Sleep(200 * time.Millisecond)
			requestCloseExplicit("ui-quit") // 明确退出：未记住时弹选择框，已记住=托盘时仍真退出（v2.0.7）
		}()
	case "close_choice":
		// v2.2.0：页面关窗确认层的选择回传（点×流程：wndproc → requestClose → closeask 推送 → 这里）
		choice := argStr(args, 0)
		remember := argBool(args, 1, false)
		go closeChoice(choice, remember)
		writeJSON(w, map[string]any{"ok": true})
	case "ui_log":
		logf("[UI] %s", argStr(args, 0))
		writeJSON(w, map[string]any{"ok": true})
	default:
		logf("未知桥方法：%s", name)
		writeJSON(w, map[string]any{"ok": false, "err": "unknown method " + name})
	}
}

func serveAudio(app *App, w http.ResponseWriter, r *http.Request) {
	mid := atoiAny(r.URL.Query().Get("id"), 0)
	data := app.store.Get(mid)
	if data == nil {
		http.Error(w, "not ready", 404)
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func initResult(app *App) map[string]any {
	cfg := app.UICfg()
	type th struct {
		Name  string `json:"name"`
		Label string `json:"label"`
		Bg    string `json:"bg"`
		Acc   string `json:"accent"`
	}
	defs := []th{
		{"clay", "奶酪黄", "#FFF1C5", "#FF8A5C"},
		{"blue", "深海蓝", "#0b1f3a", "#3b82f6"},
		{"red", "中国红", "#2a0b0b", "#ef4444"},
		{"green", "青竹绿", "#06231a", "#10b981"},
		{"dark", "黑板黑", "#000000", "#f59e0b"},
		{"light", "浅色", "#f5f7fa", "#2563eb"},
	}
	themes := make([]map[string]any, 0, len(defs))
	for _, t := range defs {
		themes = append(themes, map[string]any{
			"name": t.Name, "label": t.Label, "bg": t.Bg, "accent": t.Acc,
		})
	}
	cls := cfgStr(app.cfg, "className")
	return map[string]any{
		"ok": true, "ver": version, "app": appName, "base": app.api.Base(), "latest": app.latestStr(),
		"cls": cls, "bound": app.Token() != "", "cfg": cfg,
		"themes": themes, "autostart": autostartOn(),
	}
}
