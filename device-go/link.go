package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var errBye = errors.New("server said bye")

// wsDebug 打开后把每一帧 WS 原文写进日志（YDC_WS_DEBUG=1）
var wsDebug = os.Getenv("YDC_WS_DEBUG") == "1"

// LinkCB 网络线程回调（只入队/改状态，不做界面阻塞操作）
type LinkCB struct {
	OnStatus  func(text string, ok bool)
	OnConfig  func(cfg map[string]any, className string)
	OnNotify  func(m map[string]any, fromPoll bool)
	OnAudio   func(mid int)
	OnBye     func(reason string)
	OnExpired func(expired bool, text string)
	OnLastID  func(mid int)
	OnCheck   func(data map[string]any, st int)
}

// Link WebSocket 实时推送 + 断开降级 HTTP 轮询（等价 Python DeviceLink）
type Link struct {
	api    *Api
	token  string
	cb     LinkCB
	stop   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	lastID int
}

func NewLink(api *Api, token string, lastID int, cb LinkCB) *Link {
	return &Link{api: api, token: token, cb: cb, stop: make(chan struct{}), lastID: lastID}
}

func (l *Link) LastID() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastID
}

func (l *Link) SetLastID(id int) {
	l.mu.Lock()
	if id > l.lastID {
		l.lastID = id
	}
	l.mu.Unlock()
}

func (l *Link) stopped() bool {
	select {
	case <-l.stop:
		return true
	default:
		return false
	}
}

func (l *Link) Stop() {
	l.once.Do(func() { close(l.stop) })
}

func (l *Link) Start() {
	go l.run()
}

// run: WS 长连接为主，异常后降级轮询一段时间，再回头重连
func (l *Link) run() {
	first := l.LastID() <= 0
	for !l.stopped() {
		err := l.wsLoop()
		if l.stopped() || errors.Is(err, errBye) {
			return
		}
		if err != nil {
			logf("WebSocket 断开：%v", err)
		}
		l.cb.OnStatus("网络异常，正在重试…", false)
		for i := 0; i < 2 && !l.stopped(); i++ {
			l.pollOnce(&first)
			time.Sleep(3 * time.Second)
		}
	}
}

func (l *Link) wsLoop() error {
	wsURL, origin := l.api.WsURL(l.token)
	h := http.Header{}
	h.Set("User-Agent", uaString)
	h.Set("Origin", origin)
	h.Set("Accept-Language", "zh-CN,zh;q=0.9")
	d := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	c, resp, err := d.Dial(wsURL, h)
	if err != nil {
		if resp != nil {
			if resp.StatusCode == 401 {
				l.cb.OnBye("unbind")
				return errBye
			}
			return fmt.Errorf("握手失败 HTTP %d", resp.StatusCode)
		}
		return err
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(90 * time.Second))
	c.SetPongHandler(func(string) error { return c.SetReadDeadline(time.Now().Add(90 * time.Second)) })
	if err := c.WriteJSON(map[string]any{"type": "hello", "version": version}); err != nil {
		return err
	}
	l.cb.OnStatus("已连接", true)
	logf("WebSocket 已连接")

	done := make(chan struct{})
	defer close(done)
	go func() { // 心跳：25s 一次 ping，防 NAT/代理断流
		tk := time.NewTicker(25 * time.Second)
		defer tk.Stop()
		for {
			select {
			case <-l.stop:
				return
			case <-done:
				return
			case <-tk.C:
				_ = c.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
				_ = c.WriteJSON(map[string]any{"type": "ping"})
			}
		}
	}()

	for {
		if l.stopped() {
			return nil
		}
		_, raw, err := c.ReadMessage()
		if err != nil {
			logf("WS 读取结束：%v", err)
			return err
		}
		if wsDebug {
			logf("WS 收到：%s", trimRunes(string(raw), 200))
		}
		_ = c.SetReadDeadline(time.Now().Add(90 * time.Second))
		var m map[string]any
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		switch fmt.Sprintf("%v", m["type"]) {
		case "notify":
			l.cb.OnNotify(m, false)
		case "audio":
			l.cb.OnAudio(atoiAny(m["id"], 0))
		case "pong":
			l.cb.OnStatus("已连接", true)
		case "bye":
			l.cb.OnExpired(false, "")
			l.cb.OnBye(fmt.Sprintf("%v", m["reason"]))
			return errBye
		}
	}
}

func (l *Link) pollOnce(first *bool) {
	path := fmt.Sprintf("/api/device/poll?since=%d&token=%s", l.LastID(), url.QueryEscape(l.token))
	st, res, err := l.api.Get(path, l.token, 20*time.Second)
	if err != nil {
		l.cb.OnStatus("网络异常，正在重试…", false)
		return
	}
	if l.cb.OnCheck != nil {
		l.cb.OnCheck(res, st)
	}
	if st == 200 && res != nil && res["ok"] != false {
		l.cb.OnStatus("轮询中", true)
		cfg, _ := res["config"].(map[string]any)
		name, _ := res["className"].(string)
		l.cb.OnConfig(cfg, name)
		if *first {
			l.SetLastID(atoiAny(res["lastId"], 0)) // 首连不回放历史
			*first = false
		}
		if arr, ok := res["messages"].([]any); ok {
			for _, it := range arr {
				m, ok := it.(map[string]any)
				if !ok {
					continue
				}
				l.SetLastID(atoiAny(m["id"], 0))
				l.cb.OnNotify(m, true)
			}
		}
		return
	}
	if st == 401 {
		l.cb.OnBye("unbind")
		l.Stop()
		return
	}
	l.cb.OnStatus("网络异常，正在重试…", false)
}
