package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// App 教室端运行状态机（等价 Python 的 WebApp）
type App struct {
	api   *Api
	hub   *Hub
	store *AudioStore

	mu       sync.Mutex
	cfg      map[string]any
	token    string
	link     *Link
	handled  map[int]bool
	reported map[int]bool // 已上报 played 的消息（防重复刷库）
	pageGot  map[int]bool // 页面已取到音频的消息（看门狗判定页面是否已在播）
	items    []*Item
	cur      *Item
	voiceSeq int
	lastID   int
	timer    *time.Timer
	status   string
	bound    bool
	closing  bool

	winShow func(show bool)
	onQuit  func()

	// v2.0.8：winVisible 查询窗口是否可见；silentStart=开机自启/--silent 启动（才允许自动藏到托盘）；
	// autoPopped=本条播报是「自动弹出」的（播完自动收回；老师自己开着的窗口不收回）
	winVisible  func() bool
	silentStart bool
	autoPopped  bool

	// v2.1.0：latest=服务端下发的最新版本（仅界面提示，不强制、不自动替换 exe）；
	// hintsOnce 保证自检轮询只起一个 goroutine；
	// baseMu 串行化「比较+切换 base」——自检轮询与主链路会并发进 applyHints
	latest    string
	hintsOnce sync.Once
	baseMu    sync.Mutex
}

type Item struct {
	ID       int
	Text     string
	Voice    bool
	Repeat   int
	Ms       int
	From     string
	Deadline int64
	PlayedAt int64 // 页面「播报完成」回执到达时刻（0 = 尚未回执）
	holdLog  bool  // 「语音未播完→顺延」日志只打一次，避免刷屏
}

// setToken 统一改令牌：App.token 与 Api.token 必须同步。
// 历史缺陷（v2.2.6 及之前）：只赋 a.token，Api.token 恒空 → Api.Status() 的
// /api/device/status 一直带空令牌 → 恒 401，服务端下发的 base/latest 永远收不到。
// 调用方若已持 a.mu，本函数不再加锁（只写这两个字段）。
func (a *App) setToken(t string) {
	a.token = t
	if a.api != nil {
		a.api.SetToken(t)
	}
}

func NewApp(api *Api, hub *Hub, cfg map[string]any) *App {
	a := &App{
		api: api, hub: hub, store: NewAudioStore(), cfg: cfg,
		handled: map[int]bool{}, reported: map[int]bool{}, pageGot: map[int]bool{},
		lastID: atoiAny(cfg["lastId"], 0),
	}
	a.setToken(cfgStr(cfg, "token"))
	go a.lockWatchdog()
	return a
}

// lockWatchdog 盯住状态锁：连续 30s 抢不到就记日志。
// 教室端是无人值守设备，静默卡死（如锁重入）必须留下线索而不是无声宕住。
func (a *App) lockWatchdog() {
	bad := 0
	for range time.Tick(10 * time.Second) {
		if a.mu.TryLock() {
			a.mu.Unlock()
			bad = 0
			continue
		}
		bad++
		if bad == 3 {
			logf("警告：状态锁连续 30s 未释放，疑似死锁或长时间阻塞，请把日志发回")
		}
	}
}

// ---------- 配置 ----------

// cfgIntLocked 读取配置（调用者必须已持有 a.mu）
// 注意：Go 的 sync.Mutex 不可重入，持锁时只能用本函数，绝不能再调用 cfgInt
func (a *App) cfgIntLocked(key string, def int) int {
	if a.cfg == nil {
		return def
	}
	if v, ok := a.cfg[key]; ok && v != nil {
		return atoiAny(v, def)
	}
	return def
}

func (a *App) cfgInt(key string, def int) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfgIntLocked(key, def)
}

func (a *App) UICfg() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := coalesceCfg(a.cfg)
	d := map[string]any{
		"theme":          c["theme"],
		"displaySeconds": c["displaySeconds"],
		"fontSize":       c["fontSize"],
		"repeat":         c["repeat"],
		"beep":           c["beep"],
		"localTts":       c["localTts"],
		"showClock":      c["showClock"],
		"autoPopup":      c["autoPopup"],
		"className":      cfgStr(a.cfg, "className"),
		"token":          boolInt(a.token != ""),
	}
	return d
}

func coalesceCfg(raw map[string]any) map[string]any {
	c := map[string]any{}
	for k, v := range raw {
		c[k] = v
	}
	def := map[string]any{
		"theme": "clay", "displaySeconds": 20, "fontSize": "", "repeat": 1,
		"beep": 1, "localTts": 1, "showClock": 1, "autoPopup": 1,
	}
	for k, v := range def {
		if _, ok := c[k]; !ok {
			c[k] = v
		}
	}
	if atoiAny(c["displaySeconds"], 20) <= 0 {
		c["displaySeconds"] = 20
	}
	if atoiAny(c["repeat"], 1) <= 0 {
		c["repeat"] = 1
	}
	if s, _ := c["theme"].(string); s == "" {
		c["theme"] = "clay"
	}
	return c
}

func (a *App) SetConfig(patch map[string]any) map[string]any {
	a.mu.Lock()
	for k, v := range patch {
		a.cfg[k] = v
	}
	a.mu.Unlock()
	_ = saveConfig(a.cfg)
	cfg := a.UICfg()
	a.push(map[string]any{"t": "config", "cfg": cfg})
	return cfg
}

// ---------- 状态推送 ----------

func (a *App) push(payload map[string]any) {
	if a.hub != nil {
		a.hub.Send(payload)
	}
}

func (a *App) show(text string, ok bool) {
	a.mu.Lock()
	if a.status == text {
		a.mu.Unlock()
		return
	}
	a.status = text
	a.mu.Unlock()
	a.push(map[string]any{"t": "status", "text": text, "ok": ok})
}

func (a *App) toast(text string, err bool) {
	a.push(map[string]any{"t": "toast", "text": text, "err": err})
}

// ---------- 网络事件 ----------

func (a *App) onStatus(text string, ok bool) { a.show(text, ok) }

func (a *App) onConfig(cfg map[string]any, className string) {
	if cfg == nil {
		return
	}
	a.mu.Lock()
	for k, v := range cfg {
		a.cfg[k] = v
	}
	if className != "" {
		a.cfg["className"] = className
	}
	a.mu.Unlock()
	_ = saveConfig(a.cfg)
	a.push(map[string]any{"t": "config", "cfg": a.UICfg()})
}

func (a *App) onAudio(mid int) {
	if mid <= 0 || a.store.Get(mid) != nil {
		return
	}
	go a.loadCloudAudio(mid)
}

func (a *App) onBye(reason string) {
	if reason == "" {
		reason = "unbind"
	}
	a.mu.Lock()
	a.setToken("")
	a.cfg["token"] = ""
	a.cfg["lastId"] = nil
	a.cfg["classId"] = nil
	a.cfg["className"] = ""
	l := a.link
	a.link = nil
	a.mu.Unlock()
	if l != nil {
		l.Stop()
	}
	_ = saveConfig(a.cfg)
	// 页面契约（与 Python 版一致）：bye → 清绑定态并自动回到配对页
	a.push(map[string]any{"t": "bye", "reason": reason})
	a.show("未绑定", false)
	if a.winShow != nil {
		a.winShow(true)
	}
	logf("已被解绑：%s", reason)
}

func (a *App) onExpired(expired bool, text string) {
	// 页面读的是 p.on（Python 版同名），曾误写成 expired 导致到期横幅永不显示
	if text == "" {
		text = "账号已到期，请联系老师续期"
	}
	a.push(map[string]any{"t": "expired", "on": expired, "text": text})
}

// ---------- 通知 ----------

func (a *App) onNotify(m map[string]any, fromPoll bool) {
	mid := atoiAny(m["id"], 0)
	text := strings.TrimSpace(anyStr(m["text"]))
	if mid <= 0 || text == "" {
		return
	}
	a.mu.Lock()
	if a.handled[mid] {
		a.mu.Unlock()
		return
	}
	ms := atoiAny(m["ms"], 0)
	if ms <= 0 {
		// 持锁中：只能用 cfgIntLocked（cfgInt 会再次加锁 → 自死锁，曾导致设备收不到通知）
		ms = a.cfgIntLocked("displaySeconds", 20) * 1000
	}
	if ms < 6000 {
		ms = 6000
	}
	it := &Item{
		ID: mid, Text: text, Voice: truthy(m["voice"]),
		Repeat: clampInt(atoiAny(m["repeat"], 1), 1, 5), Ms: ms,
		From: anyStr(m["from"]), Deadline: nowMs() + int64(ms),
	}
	a.lastID = mid
	if a.cfg != nil {
		a.cfg["lastId"] = mid
	}
	a.items = append(a.items, it)
	a.mu.Unlock()
	logf("收到通知 #%d：%s", mid, trimRunes(text, 40))
	a.beep()
	a.pump()
}

func (a *App) pump() {
	a.mu.Lock()
	if a.cur != nil || len(a.items) == 0 {
		a.mu.Unlock()
		return
	}
	it := a.items[0]
	a.items = a.items[1:]
	a.cur = it
	a.handled[it.ID] = true
	a.voiceSeq++
	seq := a.voiceSeq
	a.mu.Unlock()
	a.start(it, seq)
}

func (a *App) start(it *Item, seq int) {
	if a.winShow != nil {
		// 窗口本来是藏着的 → 这次算「自动弹出」，播报结束才收回；本来就开着 → 不收回（v2.0.8）
		popped := a.winVisible != nil && !a.winVisible()
		a.winShow(true)
		a.mu.Lock()
		a.autoPopped = popped
		a.mu.Unlock()
	}
	a.push(map[string]any{
		"t": "notify", "id": it.ID, "text": it.Text, "voice": it.Voice,
		"repeat": it.Repeat, "ms": it.Ms, "from": it.From,
	})
	a.show("播报中", true)
	go a.playWatchdog(it)
	// 收到即回执：手机端立刻从「已发送」变「播报中」（与 Python 版语义一致）
	a.Ack(it.ID)
	if it.Voice {
		go a.prepareVoice(it)
	}
	a.arm(it, seq)
}

// arm 到点自动收起（语音播放中会通过 extend 顺延）
func (a *App) arm(it *Item, seq int) {
	a.mu.Lock()
	left := it.Deadline - nowMs()
	if left < 500 {
		left = 500
	}
	if a.timer != nil {
		a.timer.Stop()
	}
	a.timer = time.AfterFunc(time.Duration(left)*time.Millisecond, func() {
		a.mu.Lock()
		// 队列里还有 → 直接切下一条；否则回空闲
		stale := a.voiceSeq != seq
		empty := len(a.items) == 0
		a.mu.Unlock()
		if stale {
			return
		}
		// 语音还没播完（页面尚未回执）→ 只顺延，不收回窗口、不掐语音
		if a.holdVoice() {
			return
		}
		a.finish(empty)
	})
	a.mu.Unlock()
}

// Extend 语音正在播 → 顺延显示时间（本机兜底播放用：页面也要一起顺延，故回推页面）
func (a *App) Extend(ms int) { a.extend(ms, true) }

// ExtendFromUI 页面侧 extend(ms) 调用：页面自己已经顺延过，这里只改 Go 的定时，不再回推
// （回推会让页面把同一段时间顺延两次，进度条会跳）
func (a *App) ExtendFromUI(ms int) { a.extend(ms, false) }

func (a *App) extend(ms int, echo bool) {
	if ms <= 0 {
		return
	}
	a.mu.Lock()
	if a.cur == nil {
		a.mu.Unlock()
		return
	}
	if until := nowMs() + int64(ms); until > a.cur.Deadline {
		a.cur.Deadline = until
	}
	it, seq := a.cur, a.voiceSeq
	a.mu.Unlock()
	if echo {
		a.push(map[string]any{"t": "extend", "ms": ms})
	}
	a.arm(it, seq)
}

// holdVoice 语音尚未回执「播报完成」→ 把当前条再顺延一点，返回 true 表示「先别收场」。
// 多遍播报时显示时长往往先走完，此时收窗会把最后几遍语音掐掉（用户反馈：没播报完界面就关了）。
// 上限 = 预计语音总时长 + 30s（音频还没到手时按 180s 兜底），页面假死也不会让窗口永远挂着。
func (a *App) holdVoice() bool {
	a.mu.Lock()
	cur := a.cur
	if cur == nil || !cur.Voice || cur.PlayedAt != 0 {
		a.mu.Unlock()
		return false
	}
	base, seq := cur.Deadline, a.voiceSeq
	a.mu.Unlock()
	reps := cur.Repeat
	if reps < 1 {
		reps = 1
	}
	capMs := int64(180000)
	if d := a.store.Get(cur.ID); d != nil {
		if sec := wavDuration(d); sec > 0 {
			capMs = int64(sec*float64(reps)*1000) + 30000
		}
	}
	if nowMs() >= base+capMs {
		logf("#%d 语音超过 %.0fs 仍无播报回执，按超时收口", cur.ID, float64(capMs)/1000)
		return false
	}
	a.mu.Lock()
	if a.cur == cur {
		cur.Deadline = nowMs() + 800
		quiet := cur.holdLog
		cur.holdLog = true
		a.mu.Unlock()
		if !quiet {
			logf("#%d 语音还在播报，窗口顺延不收", cur.ID)
		}
	} else {
		a.mu.Unlock()
	}
	a.arm(cur, seq)
	return true
}

// finish 播完一条（ok 由 UI 回执决定），继续下一条或回空闲
func (a *App) finish(goIdle bool) {
	a.mu.Lock()
	cur := a.cur
	a.cur = nil
	a.mu.Unlock()
	if cur != nil {
		a.push(map[string]any{"t": "done", "id": cur.ID})
		if !cur.Voice {
			// 纯文字通知：显示时间走完即「已播报」（语音由页面播放/本机兜底决定）
			a.postPlayed(cur.ID, true)
		}
	}
	a.mu.Lock()
	more := len(a.items) > 0
	popped := a.autoPopped
	a.mu.Unlock()
	if more {
		a.pump()
		return
	}
	// v2.0.8：只有「自动弹出」的窗口才在播完自动收回；老师手动打开着的窗口不再被收走
	if popped && a.cfgInt("autoPopup", 1) != 0 && a.winShow != nil && a.token != "" {
		a.winShow(false)
		a.mu.Lock()
		a.autoPopped = false
		a.mu.Unlock()
	}
	a.show("已就绪，等待老师发送通知", true)
}

// ---------- 语音 ----------

func (a *App) prepareVoice(it *Item) {
	if d := readCache(it.ID, it.Text); d != nil {
		a.store.Put(it.ID, d)
		logf("#%d 命中本地音频缓存", it.ID)
		return
	}
	if a.cfgInt("localTts", 1) != 0 {
		if d := synthLocal(it.Text); d != nil {
			writeCache(it.ID, it.Text, d)
			a.store.Put(it.ID, d)
			logf("#%d 本机合成完成 %.1fs", it.ID, wavDuration(d))
			return
		}
	}
	a.loadCloudAudio(it.ID)
}

func (a *App) loadCloudAudio(mid int) {
	for i := 0; i < 10; i++ {
		if a.store.Get(mid) != nil {
			return
		}
		if d := a.api.FetchAudio(mid, a.Token()); d != nil {
			writeCache(mid, "", d)
			a.store.Put(mid, d)
			logf("#%d 云端音频就绪 %.1fs", mid, wavDuration(d))
			return
		}
		time.Sleep(900 * time.Millisecond)
	}
	logf("#%d 音频未取到", mid)
}

// playWatchdog 回执看门狗：手机端状态不能永远停在“播报中”。
// 页面（Edge 壳）负责播放与回执；页面被挂起、播放被打断、回执请求丢失时，
// 这里兜底收口：已取到音频→视为已播报；从未取到音频→转本机 winmm 播放。
func (a *App) playWatchdog(it *Item) {
	reps := it.Repeat
	if reps < 1 {
		reps = 1
	}
	sec := 0.0
	deadline := time.Now().Add(15 * time.Second) // 页面取音+起播的正常上限
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		a.mu.Lock()
		done := a.reported[it.ID]
		a.mu.Unlock()
		if done { return }
		if d := a.store.Get(it.ID); d != nil && sec == 0 {
			sec = wavDuration(d)
			if sec <= 0 { sec = 3 }
			// 已知音频长度：给页面「取音+播放+回执」留足时间，再往后才判超时
			deadline = time.Now().Add(time.Duration((sec*float64(reps)+12)*float64(time.Second)))
		}
	}
	a.mu.Lock()
	got := a.pageGot[it.ID]
	a.mu.Unlock()
	if got {
		logf("#%d 看门狗：页面已取音但无回执，按已播报收口", it.ID)
		a.postPlayed(it.ID, true)
		return
	}
	logf("#%d 看门狗：页面未取到音频，转本机兜底", it.ID)
	go a.localPlayFallback(it.ID)
}

// markPageGot 记录页面已取到该条音频（看门狗据此判断页面是否已在播）
func (a *App) markPageGot(mid int) {
	a.mu.Lock()
	if len(a.pageGot) > 500 {
		a.pageGot = map[int]bool{}
	}
	a.pageGot[mid] = true
	a.mu.Unlock()
}

// ---------- 手机端回执 ----------

// Played 手机端「已播报/播报失败」回执入口。
// ok=false 不当场上报失败：先走本机兜底播放（网页无声/被拦/取音失败时教室也必须出声），
// 兜底结果才决定最终回执。
func (a *App) Played(mid int, ok bool) {
	if a.token == "" || mid <= 0 {
		return
	}
	if !ok {
		go a.localPlayFallback(mid)
		return
	}
	a.postPlayed(mid, true)
}

// postPlayed 上报一次播报结果（同一条只报一次，避免多次回调刷库/覆盖结果）
func (a *App) postPlayed(mid int, ok bool) {
	a.mu.Lock()
	if a.reported[mid] {
		a.mu.Unlock()
		return
	}
	if len(a.reported) > 500 { // 长跑设备防内存膨胀
		a.reported = map[int]bool{}
	}
	a.reported[mid] = true
	if a.cur != nil && a.cur.ID == mid {
		a.cur.PlayedAt = nowMs() // 播报链路已收口，窗口可以正常收起
	}
	a.mu.Unlock()
	token := a.token
	api := a.api
	go func() {
		_, _, err := api.Post("/api/device/played", map[string]any{"token": token, "id": mid, "ok": ok}, "")
		if err != nil {
			logf("played 回执失败：%v", err)
		}
	}()
}

// localPlayFallback 本机兜底播放：winmm 直接放 WAV（含重播次数），播完再回执。
func (a *App) localPlayFallback(mid int) {
	a.mu.Lock()
	cur := a.cur
	reported := a.reported[mid]
	a.mu.Unlock()
	if reported {
		return
	}
	if cur == nil || cur.ID != mid {
		// 队列已切走这条（页面回执来晚了）→ 不再补放，直接按失败收口
		a.postPlayed(mid, false)
		return
	}
	reps := cur.Repeat
	if reps < 1 {
		reps = 1
	}
	d := a.store.Get(mid)
	if d == nil {
		a.loadCloudAudio(mid) // 页面没等到音频，这里再拉一次（含云端按需合成）
		d = a.store.Get(mid)
	}
	if d == nil {
		logf("#%d 本机兜底失败：无音频", mid)
		a.postPlayed(mid, false)
		return
	}
	sec := wavDuration(d)
	if sec <= 0 {
		sec = 3
	}
	// 播放期间保持大字（与 Python 版"等音频时顺延显示"一致）
	a.Extend(int(sec*1000)*reps + 500)
	ok := true
	for i := 0; i < reps; i++ {
		if i > 0 {
			time.Sleep(150 * time.Millisecond) // 重播间隔
		}
		if !playWav(d) {
			ok = false
			break
		}
		time.Sleep(time.Duration(sec*1000+100) * time.Millisecond)
	}
	stopWav()
	res := "成功"
	if !ok {
		res = "失败"
	}
	logf("#%d 本机兜底播放%s（%.1fs x%d）", mid, res, sec, reps)
	a.postPlayed(mid, ok)
}

func (a *App) Ack(mid int) {
	if a.token == "" || mid <= 0 {
		return
	}
	token := a.token
	api := a.api
	go func() { _, _, _ = api.Post("/api/device/ack", map[string]any{"token": token, "id": mid}, "") }()
}

// ---------- 绑定 ----------

func (a *App) Bind(code, name string) map[string]any {
	code = strings.TrimSpace(code)
	if code == "" {
		return map[string]any{"ok": false, "err": "请输入配对码"}
	}
	if name == "" {
		name = localHostName()
	}
	st, res, err := a.api.Activate(code, name)
	if err != nil {
		return map[string]any{"ok": false, "err": "网络异常，请稍后再试"}
	}
	if st != 200 || !truthy(res["ok"]) {
		msg := fmt.Sprintf("%v", res["error"])
		if msg == "" || msg == "<nil>" {
			msg = "配对失败，请检查配对码"
		}
		return map[string]any{"ok": false, "err": msg, "code": st}
	}
	token := fmt.Sprintf("%v", res["token"])
	a.mu.Lock()
	a.setToken(token)
	a.cfg["token"] = token
	if res["classId"] != nil {
		a.cfg["classId"] = res["classId"]
	}
	if s, ok := res["className"].(string); ok && s != "" {
		a.cfg["className"] = s
	}
	if n, ok := res["name"].(string); ok && n != "" {
		a.cfg["name"] = n
	}
	a.cfg["boundAt"] = time.Now().Format("2006-01-02 15:04:05")
	a.mu.Unlock()
	_ = saveConfig(a.cfg)
	logf("绑定成功：%s", a.cfgStr("className"))
	a.applyHints(res) // 绑定应答同样可能带回对外地址/最新版本（此时 a.link 为空，不会重连）
	a.StartLink()
	cfg := a.UICfg()
	a.push(map[string]any{"t": "config", "cfg": cfg})
	return map[string]any{"ok": true, "cfg": cfg, "className": cfg["className"], "token": token}
}

func (a *App) Unbind() map[string]any {
	a.mu.Lock()
	token := a.token
	l := a.link
	a.link = nil
	a.setToken("")
	if a.cfg != nil {
		a.cfg["token"] = ""
		a.cfg["classId"] = nil
		a.cfg["className"] = ""
	}
	a.mu.Unlock()
	if token != "" {
		go a.api.Unbind(token)
	}
	if l != nil {
		l.Stop()
	}
	_ = saveConfig(a.cfg)
	logf("已解绑本机")
	return map[string]any{"ok": true, "cfg": a.UICfg()}
}

// ---------- 连接管理 ----------

func (a *App) StartLink() {
	a.hintsOnce.Do(func() { go a.hintsLoop() })
	a.mu.Lock()
	token := a.token
	if token == "" || a.link != nil {
		a.mu.Unlock()
		return
	}
	cb := LinkCB{
		OnStatus:  a.onStatus,
		OnConfig:  a.onConfig,
		OnNotify:  a.onNotify,
		OnAudio:   a.onAudio,
		OnBye:     a.onBye,
		OnExpired: a.onExpired,
		OnLastID: func(id int) {
			a.mu.Lock()
			a.cfg["lastId"] = id
			a.mu.Unlock()
		},
		OnCheck: a.checkState,
	}
	l := NewLink(a.api, token, atoiAny(a.cfg["lastId"], 0), cb)
	a.link = l
	a.mu.Unlock()
	l.Start()
	saveConfigDebounced(a.cfg)
}

// checkState 处理 status/poll 应答里的过期、绑定态
func (a *App) checkState(data map[string]any, st int) {
	if st == 401 {
		a.onBye("unbind")
		return
	}
	if data == nil {
		return
	}
	a.applyHints(data) // 域名/版本下发（服务端不下发时静默跳过）
	if v, ok := data["expired"]; ok && truthy(v) {
		// 服务端字段是 expiredText（曾误读 data["message"] → 到期横幅文字永远为空）
		a.onExpired(true, anyStr(data["expiredText"]))
		return
	}
	a.onExpired(false, "")
}

// ---------- 服务端下发（v2.1.0） ----------

// applyHints 应用服务端下发的基础地址与最新版本。
// base：域名迁移用；YDC_BASE 优先级最高（测试/多实例隔离需要），此时不被下发覆盖。
// latest：只驱动界面提示，不强制升级、不自动替换 exe。
func (a *App) applyHints(data map[string]any) {
	if data == nil {
		return
	}
	if b := anyStr(data["base"]); b != "" && os.Getenv("YDC_BASE") == "" {
		if !validBase(b) {
			// 不合法就整条丢弃：既不改内存也不落盘（否则重启也不自愈）
			logf("服务端下发的地址不是 http/https，已忽略：%s", b)
		} else {
			// 自检轮询与主链路可能同时到达：baseMu 保证「比较+切换」是原子的 → 只切一次
			a.baseMu.Lock()
			cur := a.api.Base()
			if b != cur {
				logf("服务端下发新域名 %s（原 %s），切换并重连", b, cur)
				a.mu.Lock()
				a.cfg["base"] = b
				a.mu.Unlock()
				a.saveCfgNow()
				a.api.SetBase(b)
			}
			a.baseMu.Unlock()
			if b != cur {
				a.relink()
			}
		}
	}
	if v := anyStr(data["latest"]); v != "" {
		a.mu.Lock()
		changed := a.latest != v
		a.latest = v
		a.mu.Unlock()
		if changed && verLess(version, v) {
			logf("发现新版本 %s（当前 v%s）", v, version)
			a.push(map[string]any{"t": "update", "latest": v, "cur": version, "url": a.dlURL()})
		}
	}
}

// dlURL 新版下载地址（跟随当前对外地址，域名迁移后自动指向新域）
func (a *App) dlURL() string {
	return a.api.Base() + "/download/yundianchuanhuatong-setup.exe"
}

// latestStr 服务端告知的最新版本（空 = 未知）
func (a *App) latestStr() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.latest
}

// relink 按当前 base 重建连接（lastId 游标保留在 a.cfg，不会重放历史消息）
func (a *App) relink() {
	a.mu.Lock()
	old := a.link
	if old == nil {
		a.mu.Unlock()
		return
	}
	a.link = nil
	a.mu.Unlock()
	old.Stop()
	a.StartLink()
}

// 自检节奏（包级变量：测试可缩短，避免跑 45 秒）
var (
	hintsFirstDelay = 45 * time.Second
	hintsInterval   = 30 * time.Minute
)

// hintsLoop 定时拉一次设备状态：WS 长连接期间也能收到域名/版本下发。
// 教室端常年不重启，只在建立连接时取一次会长期收不到迁移通知。
//
// ★401 不能 return：hintsOnce 只放行一个 goroutine，一旦退出，
// 解绑/被踢之后重新绑定就再也起不来自检（本功能的目标场景就是解绑重绑）。
func (a *App) hintsLoop() {
	first, interval := hintsFirstDelay, hintsInterval
	next := first
	unauth := false
	for {
		time.Sleep(next)
		next = interval
		if a.Token() == "" {
			unauth = false
			continue
		}
		st, res, err := a.api.Status()
		if err != nil {
			continue
		}
		if st == 401 {
			// 凭证失效由主链路（WS close/轮询 401）负责走解绑流程；这里只记一条日志并继续等
			if !unauth {
				logf("自检轮询：设备凭证已失效，暂停版本/域名自检（重新绑定后自动恢复）")
				unauth = true
			}
			continue
		}
		unauth = false
		if st == 200 {
			a.checkState(res, st) // checkState 内部会调用 applyHints
		}
	}
}

// saveCfgNow 先复制一份再落盘：a.cfg 会被多个 goroutine 并发写，
// 直接 saveConfig(a.cfg) 会让 json.Marshal 与 map 写并发（Go 直接 fatal）。
func (a *App) saveCfgNow() {
	a.mu.Lock()
	snap := make(map[string]any, len(a.cfg))
	for k, v := range a.cfg {
		snap[k] = v
	}
	a.mu.Unlock()
	_ = saveConfig(snap)
}

// ---------- 生命周期 ----------

func (a *App) UiReady() {
	logf("界面已就绪(ui_ready)")
	a.push(map[string]any{"t": "config", "cfg": a.UICfg()})
	if a.token != "" {
		a.show("连接中…", true)
		// 自动弹窗模式：只有开机自启/--silent（静默启动）才先藏到后台，有消息再弹出。
		// v2.0.8：老师手动双击打开 exe 时不再自动藏窗（用户反馈「打开就直接进托盘」）
		if a.cfgInt("autoPopup", 1) != 0 && a.silentStart {
			time.AfterFunc(1500*time.Millisecond, func() {
				a.mu.Lock()
				idle := a.cur == nil && a.token != ""
				a.mu.Unlock()
				if idle {
					hideWhenReady(true)
				}
			})
		}
	} else {
		a.show("未绑定，请输入配对码", false)
	}
}

// Token 当前绑定令牌（空 = 未绑定）
func (a *App) Token() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.token
}

// Current 当前正在播报的通知 id（无则 0）
func (a *App) Current() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cur == nil {
		return 0
	}
	return a.cur.ID
}

// ViewDone 页面倒计时走完 → 与 Go 侧定时器双保险，切下一条 / 回空闲
func (a *App) ViewDone(mid int) {
	if mid <= 0 || a.Current() != mid {
		return
	}
	if a.holdVoice() { // 页面倒计时到点、但语音还没播完：先不收场，等回执
		return
	}
	a.mu.Lock()
	empty := len(a.items) == 0
	a.mu.Unlock()
	if a.cur != nil {
		cur := a.cur
		a.mu.Lock()
		a.cur = nil
		a.mu.Unlock()
		a.push(map[string]any{"t": "done", "id": cur.ID})
	}
	a.push(map[string]any{"t": "reset"})
	if empty {
		a.finish(true)
	} else {
		a.pump()
	}
}

func (a *App) CloseWindow() {
	if a.onQuit != nil {
		a.onQuit()
	}
}

func (a *App) Quit() {
	a.mu.Lock()
	a.closing = true
	l := a.link
	a.mu.Unlock()
	if l != nil {
		l.Stop()
	}
	_ = saveConfig(a.cfg)
	if a.onQuit != nil {
		a.onQuit()
	}
}

func (a *App) beep() {
	if a.cfgInt("beep", 1) == 0 {
		return
	}
	a.push(map[string]any{"t": "beep", "times": 2})
}

func (a *App) cfgStr(key string) string { return cfgStr(a.cfg, key) }

// ---------- 小工具 ----------

func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		s := strings.ToLower(strings.TrimSpace(t))
		return !(s == "" || s == "0" || s == "false" || s == "none" || s == "<nil>")
	}
	return true
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func trimRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func cfgStr(cfg map[string]any, key string) string {
	return anyStr(cfg[key])
}

// anyStr 任意值转字符串；缺字段/nil 一律空串，避免界面出现 "<nil>"
func anyStr(v any) string {
	if v == nil {
		return ""
	}
	s := fmt.Sprintf("%v", v)
	if s == "<nil>" || s == "null" {
		return ""
	}
	return s
}
