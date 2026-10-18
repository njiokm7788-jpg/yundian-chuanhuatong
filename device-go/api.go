package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Api 带浏览器 UA 的极简 HTTP 客户端（CF 的 Browser Integrity Check 会拦默认 UA）
// base 可被服务端下发替换（域名迁移），故与 token 共用 mu 保护。
type Api struct {
	mu    sync.Mutex
	base  string
	token string
	cli   *http.Client
}

func NewApi(base string) *Api {
	return &Api{
		base: strings.TrimRight(base, "/"),
		cli:  &http.Client{Timeout: 30 * time.Second},
	}
}

// Base 当前对外基础地址
func (a *Api) Base() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.base
}

// validBase 只接受 http/https 绝对地址。服务端 [vars] PUBLIC_BASE 写错一个字符，
// 客户端会把它落盘进 config.json 且重启也不自愈（没有备用域可回退），必须在这里挡住。
func validBase(b string) bool {
	return strings.HasPrefix(b, "http://") || strings.HasPrefix(b, "https://")
}

// SetBase 切换对外基础地址（服务端下发新域名时调用；空串/非法值忽略）
func (a *Api) SetBase(b string) {
	b = strings.TrimRight(strings.TrimSpace(b), "/")
	if !validBase(b) {
		return
	}
	a.mu.Lock()
	a.base = b
	a.mu.Unlock()
}

func (a *Api) SetToken(t string) {
	a.mu.Lock()
	a.token = t
	a.mu.Unlock()
}

func (a *Api) Token() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.token
}

// Do 返回 HTTP 状态码 + 已解析 JSON（无法解析时 nil）+ 原始字节
func (a *Api) Do(path string, body map[string]any, method string, token string, timeout time.Duration) (int, map[string]any, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, nil, err
		}
		rd = bytes.NewReader(b)
	}
	if method == "" {
		if body != nil {
			method = "POST"
		} else {
			method = "GET"
		}
	}
	req, err := http.NewRequest(method, a.Base()+path, rd)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("User-Agent", uaString)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	tok := token
	if tok == "" {
		tok = a.Token()
	}
	if tok != "" {
		req.Header.Set("authorization", "Bearer "+tok)
	}
	cli := a.cli
	if timeout > 0 {
		cli = &http.Client{Timeout: timeout}
	}
	resp, err := cli.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return resp.StatusCode, m, raw, nil
}

func (a *Api) Get(path, token string, timeout time.Duration) (int, map[string]any, error) {
	st, m, _, err := a.Do(path, nil, "GET", token, timeout)
	return st, m, err
}

func (a *Api) Post(path string, data map[string]any, token string) (int, map[string]any, error) {
	if data == nil {
		data = map[string]any{}
	}
	st, m, _, err := a.Do(path, data, "POST", token, 20*time.Second)
	return st, m, err
}

func (a *Api) Activate(code, name string) (int, map[string]any, error) {
	return a.Post("/api/device/activate", map[string]any{"code": code, "name": name}, "")
}

func (a *Api) Unbind(token string) (int, map[string]any, error) {
	return a.Post("/api/device/unbind", map[string]any{}, token)
}

func (a *Api) ReportPlayed(mid int, ok bool) {
	_, _, err := a.Post("/api/device/played", map[string]any{"id": mid, "ok": ok}, "")
	if err != nil {
		logf("played 回执失败 #%d %v", mid, err)
	}
}

func (a *Api) Ack(mid int) {
	_, _, err := a.Post("/api/device/ack", map[string]any{"id": mid}, "")
	if err != nil {
		logf("ack 失败 #%d %v", mid, err)
	}
}

func (a *Api) Status() (int, map[string]any, error) {
	path := "/api/device/status?token=" + url.QueryEscape(a.Token())
	return a.Get(path, a.Token(), 15*time.Second)
}

// FetchAudio 拉取云端合成好的 WAV（>100 字节才算有效）
func (a *Api) FetchAudio(mid int, token string) []byte {
	path := fmt.Sprintf("/api/audio/%d?token=%s", mid, url.QueryEscape(token))
	st, _, raw, err := a.Do(path, nil, "GET", token, 60*time.Second)
	if err != nil || st != 200 || len(raw) <= 100 {
		return nil
	}
	if !bytes.HasPrefix(raw, []byte("RIFF")) {
		return nil
	}
	return raw
}

func (a *Api) WsURL(token string) (string, string) {
	base := a.Base()
	u := strings.Replace(base, "https://", "wss://", 1)
	u = strings.Replace(u, "http://", "ws://", 1)
	if !strings.HasPrefix(u, "ws") {
		u = "wss://" + u
	}
	return u + "/api/device/ws?token=" + url.QueryEscape(token), base
}
