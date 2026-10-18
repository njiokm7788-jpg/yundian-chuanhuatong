package main

// 回归测试：多遍语音播报时「界面不许提前收场」（用户反馈的 bug 1）
// 用真实定时器跑，锚定的是用户可见终态：窗口/视图有没有切回待机。

import (
	"testing"
	"time"
)

// 造一个「语音未回执」的当前条；api 指向打不通的地址，回执上报失败只会记日志
func holdApp(it *Item) *App {
	a := NewApp(NewApi("http://127.0.0.1:1"), nil, map[string]any{})
	a.token = "t-test"
	a.api.SetToken("t-test")
	a.mu.Lock()
	if it != nil {
		a.cur = it
	}
	a.mu.Unlock()
	return a
}

// 显示 0.6s、要播 3 遍（约 1.8s + 延迟）——旧逻辑 0.6s 就把窗口收了
func voiceItem(id int) *Item {
	return &Item{ID: id, Text: "测试通知", Voice: true, Repeat: 3, Ms: 600, Deadline: nowMs() + 600}
}

// 计时到点但语音还没回执：视图必须留在当前条，且显示时间被顺延
func TestViewDoneHoldsWhenVoiceUnacked(t *testing.T) {
	it := voiceItem(9001)
	a := holdApp(it)
	before := it.Deadline
	a.ViewDone(it.ID)
	if a.Current() != it.ID {
		t.Fatalf("语音未回执时 view_done 不该收场，当前=%d", a.Current())
	}
	if it.Deadline <= before {
		t.Fatalf("显示时间没有顺延：%d -> %d", before, it.Deadline)
	}
}

// 定时器真到点也不许收场，回执后才收（用户看到的「多遍播报被掐掉」主场景）
func TestTimerHoldsUntilVoiceDone(t *testing.T) {
	it := voiceItem(9003)
	a := holdApp(it)
	a.arm(it, a.voiceSeq)
	time.Sleep(700 * time.Millisecond) // 原定时已到点，语音还剩两遍
	if a.Current() != it.ID {
		t.Fatalf("定时到点就把窗口收了，当前=%d", a.Current())
	}
	time.Sleep(1300 * time.Millisecond)
	if a.Current() != it.ID {
		t.Fatalf("持续顺延失效，当前=%d", a.Current())
	}
	a.Played(it.ID, true)
	deadline := time.Now().Add(3 * time.Second)
	for a.Current() != 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if a.Current() != 0 {
		t.Fatalf("播报回执后仍未收场，当前=%d", a.Current())
	}
}

// 页面回执后：view_done 正常收场（别把修复做成「永不收口」）
func TestViewDoneAfterAckCloses(t *testing.T) {
	it := voiceItem(9002)
	a := holdApp(it)
	a.Played(it.ID, true)
	if it.PlayedAt == 0 {
		t.Fatal("播报回执没落到 PlayedAt")
	}
	a.ViewDone(it.ID)
	if a.Current() != 0 {
		t.Fatalf("已回执的条目该收场，当前=%d", a.Current())
	}
}

// 页面假死也不能让窗口永远挂着：超过预计总时长 + 30s 就按超时收口
func TestHoldVoiceGivesUpAtCap(t *testing.T) {
	it := voiceItem(9004)
	a := holdApp(it)
	a.mu.Lock()
	it.Deadline = nowMs() - 200000
	a.mu.Unlock()
	if a.holdVoice() {
		t.Fatal("超过上限仍不收口，窗口会永远挂着")
	}
}

// 纯文字通知（无语音）不该被顺延钩子拦住
func TestTextItemNotHeld(t *testing.T) {
	it := voiceItem(9005)
	it.Voice = false
	a := holdApp(it)
	if a.holdVoice() {
		t.Fatal("纯文字通知不该被顺延钩子拦住")
	}
}

// 页面 extend(ms)（语音要顺延）：Go 侧定时必须跟着顺延
func TestExtendFromUIExtends(t *testing.T) {
	it := voiceItem(9006)
	a := holdApp(it)
	now := nowMs()
	a.ExtendFromUI(5000)
	if it.Deadline < now+4900 {
		t.Fatalf("页面 extend 没生效：now=%d 截止=%d", now, it.Deadline)
	}
}
