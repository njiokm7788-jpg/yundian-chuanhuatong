// 云点传话筒 · 教室端 —— 关窗流程（v2.2.0，WebView2 内嵌壳版）
//
// 界面窗口是本进程自己的 HWND（shell_windows.go），WM_CLOSE 在 wndproc 里拦截。
// 确认框不再用原生 Win32 对话框（closedlg_windows.go 已删），改由页面内 HTML 覆盖层承担：
//
//	Go 收到关窗请求 → RPC 推送 {t:'closeask'} 给页面 → 页面弹覆盖层 →
//	用户选择 → RPC close_choice(choice, remember) → Go 执行 tray/quit
//
// 可勾选“记住我的选择”，下次不再询问（写入配置 closeAsk=false, closeAction=tray|quit）。
// 明确退出入口（托盘菜单/界面退出）不受「记住=托盘」改写（v2.0.7 规则保留）。
//
// v2.2.2 兜底：推送 closeask 后启动看门狗，页面在 closeAskWatchdog 内没有任何回传
// （覆盖层没弹出来 / 页面脚本报错）就按来源兜底——明确退出入口真退出，点 × 只复位。
// 修前实测过：页面弹层失败时 closeBusy 永久卡在 1，之后点×、托盘「关闭程序」全被静默吞掉。
// 同时 endClose() 负责复位 shellClosed，否则「× → 最小化到托盘 → 重开 → 再点×」会被
// wndproc 直接吞掉（同一个「点×没反应」症状的另一条路径）。
package main

import (
	"os"
	"sync"
	"sync/atomic"
	"time"
)

var (
	mainApp     *App  // 由 main() 注入，供配置读写 / 退出使用
	srvPort     int   // 本机界面端口（壳窗口导航用）
	closeBusy   int32 // 正在处理关窗请求，避免重复弹层
	trayMode    int32 // 1=用户选择“最小化到托盘”，此时不自动弹窗
	showRetries int32 // “启动时窗口还没出现”的重试计数
	quitting    int32 // 1=正在退出
)

// scheduleShowRetry 启动阶段窗口尚未出现时的有界重试（最多 30 秒）
func scheduleShowRetry() {
	if atomic.LoadInt32(&trayMode) != 0 {
		return
	}
	if atomic.AddInt32(&showRetries, 1) > 15 {
		return
	}
	logf("界面窗口未就绪，稍后重试显示")
	time.AfterFunc(2*time.Second, func() {
		if atomic.LoadInt32(&trayMode) != 0 {
			return
		}
		setWinVisible(true)
	})
}

func resetShowRetry() { atomic.StoreInt32(&showRetries, 0) }

func appCfgGet(key string) any {
	if mainApp == nil {
		return nil
	}
	mainApp.mu.Lock()
	defer mainApp.mu.Unlock()
	return mainApp.cfg[key]
}

func closeAskEnabled() bool {
	v := appCfgGet("closeAsk")
	if v == nil {
		return true
	}
	return truthy(v)
}

func saveClosePref(action string, remember bool) {
	if mainApp == nil {
		return
	}
	patch := map[string]any{"closeAction": action}
	if remember {
		patch["closeAsk"] = false
	}
	mainApp.SetConfig(patch)
}

// beginClose 关窗请求去重入口；返回 false = 已有请求在处理，忽略本次
func beginClose() bool {
	return atomic.CompareAndSwapInt32(&closeBusy, 0, 1)
}

func endClose() {
	atomic.StoreInt32(&closeBusy, 0)
	shellCloseCancelled()
}

// releaseCloseIfBusy 看门狗专用：确实还卡在处理中才复位，避免误放掉新请求的锁
func releaseCloseIfBusy() bool {
	if !atomic.CompareAndSwapInt32(&closeBusy, 1, 0) {
		return false
	}
	shellCloseCancelled()
	return true
}

// ---------------------------------------------------------------- 关窗看门狗（v2.2.2）

const closeAskWatchdog = 15 * time.Second

var (
	closeWatchMu  sync.Mutex // 保护 closeWatch / closeWatchSeq
	closeWatch    *time.Timer
	closeWatchSeq int64
)

// armCloseWatchdog 推送 closeask 后的兜底：页面在 closeAskWatchdog 内没有任何回传即视为
// 页面不可用。此时「明确退出入口」（explicit）按用户本意真退出——保证程序永远关得掉；
// 点 × 这类非明确入口只按「取消」复位（不退出、不进托盘），保持运行。
func armCloseWatchdog(origin string, explicit bool) {
	closeWatchMu.Lock()
	defer closeWatchMu.Unlock()
	if closeWatch != nil {
		closeWatch.Stop()
	}
	closeWatchSeq++
	seq := closeWatchSeq
	closeWatch = time.AfterFunc(closeAskWatchdog, func() {
		closeWatchMu.Lock()
		if closeWatchSeq != seq { // 已有更新的关窗请求，交给它自己的看门狗
			closeWatchMu.Unlock()
			return
		}
		closeWatch = nil
		released := releaseCloseIfBusy() // 锁内判定，避免与新的关窗请求抢锁
		closeWatchMu.Unlock()
		if !released {
			return // 页面已正常回传，closeChoice 处理过了
		}
		if explicit {
			logf("关闭确认层 %v 无回传，明确退出入口按「退出」兜底（来源 %s）", closeAskWatchdog, origin)
			doQuitApp()
			return
		}
		logf("关闭确认层 %v 无回传，按「取消」复位（来源 %s；页面可能未响应）", closeAskWatchdog, origin)
	})
}

// stopCloseWatchdog 页面已回传选择，撤销兜底
func stopCloseWatchdog() {
	closeWatchMu.Lock()
	if closeWatch != nil {
		closeWatch.Stop()
		closeWatch = nil
	}
	closeWatchSeq++
	closeWatchMu.Unlock()
}

// requestClose 统一的“关闭”入口：点 ×（wndproc WM_CLOSE）、界面启动失败都走这里。
// closeAsk=true 时把确认交给页面覆盖层（异步）；否则按记住的选择直接处理。
// explicit=true 表示「明确退出入口」（托盘关闭程序 / 界面退出按钮）：页面无响应时兜底真退出。
func requestClose(origin string, explicit bool) {
	if atomic.LoadInt32(&quitting) != 0 {
		logf("正在退出，忽略关闭请求（来源 %s）", origin)
		return
	}
	if !beginClose() {
		logf("关闭确认已在处理中，忽略重复请求（来源 %s）", origin)
		return
	}

	if !closeAskEnabled() {
		act := anyStr(appCfgGet("closeAction"))
		logf("按已记住的选择处理关闭：%s（来源 %s）", act, origin)
		endClose()
		if act == "quit" {
			doQuitApp()
		} else {
			toTrayMode(origin)
		}
		return
	}

	// 弹页面内确认层。选完由 closeChoice() 回调（closeBusy 在那时释放）
	if mainApp != nil {
		// 先挂兜底再推送：Hub.Send 可能阻塞（每个订阅者最多 2s）甚至 panic，
		// 排在它后面会让兜底被绕过，closeBusy/shellClosed 又永久卡住
		armCloseWatchdog(origin, explicit)
		mainApp.hub.Send(map[string]any{"t": "closeask"})
		logf("已请求页面弹出关闭确认层（来源 %s）", origin)
	} else {
		// 页面通道不可用（极端情况）：按取消处理，保持运行
		endClose()
		logf("页面通道不可用，取消关闭（来源 %s）", origin)
	}
}

// closeChoice 页面确认层的选择回传（choice = "tray" / "quit" / "" 取消）
func closeChoice(choice string, remember bool) {
	if atomic.LoadInt32(&quitting) != 0 {
		logf("正在退出，忽略页面回传（choice=%q）", choice)
		return
	}
	stopCloseWatchdog()
	logf("关闭确认：choice=%q remember=%v", choice, remember)
	endClose()
	switch choice {
	case "quit":
		saveClosePref("quit", remember)
		doQuitApp()
	case "tray":
		saveClosePref("tray", remember)
		toTrayMode("page-confirm")
	default:
		logf("用户取消关闭，继续运行")
	}
}

// requestCloseExplicit 明确退出入口：托盘菜单「关闭程序」/ 界面内「退出」按钮。
// 用户点的就是“退出”，因此不允许被「记住我的选择=最小化到托盘」改写成“进托盘”（v2.0.7 修）。
// closeAsk=true（未记住）时仍弹同一个确认层，保持与点 × 一致的交互。
func requestCloseExplicit(origin string) {
	if closeAskEnabled() {
		requestClose(origin, true)
		return
	}
	if act := anyStr(appCfgGet("closeAction")); act != "quit" {
		logf("明确退出入口，忽略已记住的 %q（来源 %s）", act, origin)
	}
	doQuitApp()
}

func doQuitApp() {
	logf("关闭程序（保存配置后退出）")
	atomic.StoreInt32(&quitting, 1)
	trayRemove()
	if mainApp != nil {
		mainApp.Quit() // 保存配置 → onQuit → os.Exit(0)
		return
	}
	os.Exit(0)
}

func toTrayMode(origin string) {
	atomic.StoreInt32(&trayMode, 1)
	setWinVisible(false)
	logf("已最小化到托盘（来源 %s），课间推送照常接收", origin)
	trayNotify("云点传话筒", "已最小化到托盘：双击图标打开界面，右键图标可关闭程序")
}

func showShellWindow(origin string) {
	logf("打开界面（来源 %s）", origin)
	ensureShellVisible()
}

func ensureShellVisible() {
	atomic.StoreInt32(&trayMode, 0) // 用户主动要打开界面，解除托盘静默
	setWinVisible(true)
}

// showForApp 供 app 层显示/隐藏界面的入口（收到通知自动弹出、解绑后回配对页、播完自动收回）。
// v2.2.0：窗口常驻（托盘模式只是隐藏），不再有“窗口被 Edge 回收要重建”的分支。
func showForApp(show bool) {
	if !show {
		setWinVisible(false)
		return
	}
	setWinVisible(true)
}

