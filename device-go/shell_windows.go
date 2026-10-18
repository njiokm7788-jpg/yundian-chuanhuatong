// 云点传话筒 · 教室端 —— WebView2 内嵌壳（v2.2.0 起替代 Edge --app 外部壳）
//
// 为什么不用 jchv/go-webview2 的高层包装：它的 wndproc 对 WM_CLOSE 直接 DestroyWindow（拦不住），
// 且创建时强制 ShowWindow（静默启动会闪窗）。本文件自建窗口类 + 直接用公开导出的
// pkg/edge.Chromium.Embed(hwnd)，窗口生命周期完全归本进程管：
//
//	点 ×（WM_CLOSE）→ 自己的 wndProc 拦截 → requestClose("x-close") → 页面内确认层/托盘
//	窗口创建后常驻（托盘模式只是 Hide，不销毁）→ 不再有"窗口被 Edge 回收要重建"的问题
//	单实例 = 全局命名 mutex，替代旧的跨进程窗口探测
//
// 尖兵验证记录（device-go/_spike_wv2/）：WM_CLOSE 拦截 / SW_HIDE 创建 / 置顶 / 渲染 全 PASS。
// 坑：消息循环退出必须 PostThreadMessageW(主线程, WM_QUIT)；PostQuitMessage 从定时器
// goroutine 调用是投到别的线程队列，循环永远退不出（尖兵期实测踩过）。
package main

import (
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/jchv/go-webview2/pkg/edge"
)

// wmClose/wmDestroy/swShow 等常量在 window_windows.go 声明，此处只补壳专属的
const (
	wmQuit        = 0x0012
	wmSize        = 0x0005 // 窗口尺寸变化：必须同步 PutBounds，否则 WebView2 渲染区不跟随
	swHide        = 0
	swRestore     = 9
	hwndTopmost   = ^uintptr(0) // HWND_TOPMOST
	swpNoSize     = 0x0001
	swpNoMove     = 0x0002
	swpNoActivate = 0x0010
	swpShowWindow = 0x0040
	wsOverlappedWindow = 0x00CF0000

	classMain = "YundianChuanhuatongMain"
)

type msgT struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      struct{ x, y int32 }
}

type wndClassExT struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

var (
	wvMu        sync.Mutex // 保护 chromium / hwndMain：显示、隐藏、退出可能来自不同 goroutine
	chromium    *edge.Chromium
	hwndMain    uintptr
	shellReady  bool   // Embed 完成，页面可导航
	mainThread  uintptr
	shellClosed int32  // 1=用户已通过页面确认层选择关闭（等待 quit），避免重复处理
)

// navDone 由 NavigationCompleted 回调置位（页面加载完成的唯一可靠信号）
var navDone = make(chan struct{})

func utf16PtrW(s string) *uint16 { p, _ := syscall.UTF16PtrFromString(s); return p }

// mainWndProc 主窗口过程：WM_CLOSE 拦截是本方案的命根子——不销毁，走关窗选择流程
func mainWndProc(h, m, wp, lp uintptr) uintptr {
	switch m {
	case wmClose:
		logf("收到窗口关闭消息（点×），进入关闭选择")
		if atomicCAS(&shellClosed, 0, 1) {
			go func() { requestClose("x-close", false) }()
		}
		return 0
	case wmSize:
		// 直接用 pkg/edge 就得自己同步 controller bounds（jchv 高层包装在 wmSize 里做，
		// 这里漏了就是白屏：页面加载正常但渲染区 0×0）
		wvMu.Lock()
		ch := chromium
		wvMu.Unlock()
		if ch != nil {
			ch.Resize()
		}
		return 0
	case wmDestroy:
		procPostQuitMsgW.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(h, m, wp, lp)
	return r
}

func atomicCAS(p *int32, old, new int32) bool {
	return atomic.CompareAndSwapInt32(p, old, new)
}

// createShellWindow 创建主窗口（隐藏）并嵌入 WebView2；返回错误即壳不可用
func createShellWindow(port int) error {
	wvMu.Lock()
	defer wvMu.Unlock()
	if hwndMain != 0 {
		return nil
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	mainThread, _, _ = procGetCurrentThreadId.Call()

	hInst, _, _ := procGetModuleHandleW.Call(0)
	wc := wndClassExT{
		cbSize:        uint32(unsafe.Sizeof(wndClassExT{})),
		lpfnWndProc:   syscall.NewCallback(mainWndProc),
		hInstance:     hInst,
		hIcon:         trayLoadIcon(hInst),
		hIconSm:       trayLoadIcon(hInst),
		lpszClassName: utf16PtrW(classMain),
	}
	if r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		if e, ok := err.(syscall.Errno); !ok || uintptr(e) != errClassAlreadyExists {
			return fmt.Errorf("注册窗口类失败：%v", err)
		}
	}

	// 不带 WS_VISIBLE 创建 → 窗口先隐藏，Embed 完成后按需显示（不闪窗），
	// 这替代了 Edge 壳时代的「屏幕外预建」整套 hack
	hwnd, _, err := procCreateWindowExW.Call(0,
		uintptr(unsafe.Pointer(utf16PtrW(classMain))),
		uintptr(unsafe.Pointer(utf16PtrW(appName))),
		wsOverlappedWindow,
		100, 60, 1280, 780,
		0, 0, hInst, 0)
	if hwnd == 0 {
		return fmt.Errorf("创建窗口失败：%v", err)
	}
	hwndMain = hwnd

	chromium = edge.NewChromium()
	chromium.DataPath = filepath.Join(confDir, "webview2")
	chromium.NavigationCompletedCallback = func(_ *edge.ICoreWebView2, _ *edge.ICoreWebView2NavigationCompletedEventArgs) {
		select {
		case <-navDone:
		default:
			close(navDone)
		}
	}
	if !chromium.Embed(hwnd) {
		hwndMain = 0
		return fmt.Errorf("WebView2 运行时不可用（Embed 失败）")
	}
	shellReady = true
	chromium.Resize() // Embed 后 controller bounds 默认 0×0，不 PutBounds 就是白屏
	chromium.Navigate(shellURL(port))
	return nil
}

func shellURL(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d/ui/", port)
}

// launchShell 语义与旧版一致（main.go / relaunchShell 调用点兼容）：
// 创建窗口（隐藏）+ 导航。startHidden 只影响之后是否立即显示。
func launchShell(port int, startHidden bool) error {
	if err := createShellWindow(port); err != nil {
		return err
	}
	logf("已创建 WebView2 界面窗口（内嵌壳 v2.2.0）")
	// v2.2.3 白屏修复：内嵌壳必须在「窗口真正显示」时完成页面加载才会出帧。
	// 原实现让窗口藏着加载：页面其实加载成功（ui_ready/SSE 正常）却永远不渲染；
	// 2026-10-15 逐项实测——事后改尺寸、切 PutIsVisible、隐藏再显示、重新导航、
	// 离屏加载、以及「屏幕内但不置顶/不激活」的显示，全部无效；只有走正常显示路径
	// （setWinVisible：屏幕内 + 置顶 + 激活）之后再加载才有效。
	// 因此静默启动也先正常显示一次让页面加载，就绪后由 hideWhenReady 立即隐藏
	//（实测显示与隐藏在同一秒内完成，老师至多看到一闪）。
	setWinVisible(true)
	if startHidden {
		hideWhenReady(true)
	}
	return nil
}

// setWinVisible 显示/隐藏界面窗口（窗口常驻，不再有"找不到就重建"分支）
func setWinVisible(show bool) {
	wvMu.Lock()
	h := hwndMain
	ch := chromium
	wvMu.Unlock()
	if h == 0 {
		if show {
			scheduleShowRetry()
		}
		return
	}
	resetShowRetry()
	if show {
		if ch != nil {
			_ = ch.Show()
		}
		procShowWindow.Call(h, swShow)
		if r, _, _ := procIsIconic.Call(h); r != 0 {
			procShowWindow.Call(h, swRestore)
		}
		centerWindow(h)
		procSetWindowPos.Call(h, hwndTopmost, 0, 0, 0, 0, swpNoSize|swpNoMove|swpShowWindow)
		procSetForegroundWindow.Call(h)
		logf("界面已弹出")
	} else {
		procShowWindow.Call(h, swHide)
		logf("界面已隐藏到后台")
	}
}

// centerWindow 居中（保持尺寸不变）
func centerWindow(h uintptr) {
	var rc rectT
	if r, _, _ := procGetWindowRect.Call(h, uintptr(unsafe.Pointer(&rc))); r == 0 {
		return
	}
	w, hh := int(rc.Right-rc.Left), int(rc.Bottom-rc.Top)
	sw, _, _ := procGetSystemMetrics.Call(smCXScreen)
	sh, _, _ := procGetSystemMetrics.Call(smCYScreen)
	x, y := (int(int32(sw))-w)/2, (int(int32(sh))-hh)/2
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	procSetWindowPos.Call(h, 0, uintptr(x), uintptr(y), 0, 0, swpNoSize|swpNoZOrder|swpNoActivate)
}

// windowVisible 窗口当前是否可见（v2.0.8 的「自动弹出判定」依赖它；窗口常驻后不再有屏幕外状态）
func windowVisible() bool {
	wvMu.Lock()
	h := hwndMain
	wvMu.Unlock()
	if h == 0 {
		return false
	}
	r, _, _ := procIsWindowVisible.Call(h)
	return r != 0
}

// hideWhenReady 等页面加载完成再隐藏（静默启动时老师不该看见窗口一闪）
func hideWhenReady(notify bool) {
	go func() {
		select {
		case <-navDone:
			// v2.2.3：导航完成 ≠ 首帧已出，留一点时间让合成器出第一帧再隐藏，
			// 否则又退回「藏着的窗口永远不渲染」那个白屏状态
			time.Sleep(500 * time.Millisecond)
		case <-time.After(6 * time.Second):
		}
		if hideWindowIfVisible() && notify {
			trayNotify("云点传话筒已在后台运行", "有消息会自动弹出；双击托盘图标可打开界面")
		}
	}()
}

// hideWindowIfVisible 只在窗口当前可见时隐藏；返回是否真的隐藏了（用于气泡去重）
func hideWindowIfVisible() bool {
	wvMu.Lock()
	h := hwndMain
	wvMu.Unlock()
	if h == 0 {
		return false
	}
	if r, _, _ := procIsWindowVisible.Call(h); r == 0 {
		return false
	}
	setWinVisible(false)
	return true
}

// closeShellForQuit 退出收尾：内嵌壳就是本进程自己的窗口，销毁即可（v2.0.9 的
// 跨进程 Edge 收尾/按 pid 杀树问题在此结构性消失）
func closeShellForQuit() {
	wvMu.Lock()
	h := hwndMain
	wvMu.Unlock()
	if h == 0 {
		logf("退出前无界面窗口")
		return
	}
	procDestroyWindow.Call(h)
	logf("界面窗口已销毁")
}

// shellCloseCancelled 页面确认层选了「取消」→ 允许下一次关窗请求再次弹层
func shellCloseCancelled() {
	atomic.StoreInt32(&shellClosed, 0)
}

// runShellMessageLoop 主消息循环（阻塞；WM_QUIT 退出）。在 main() 的 goroutine 里调用。
func runShellMessageLoop() {
	runtime.LockOSThread()
	var msg msgT
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(r) <= 0 {
			return
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}
}

// postShellQuit 从任意 goroutine 请求主消息循环退出（PostQuitMessage 只投当前线程，必须用 PostThreadMessageW）
func postShellQuit() {
	if mainThread != 0 {
		procPostThreadMessageW.Call(mainThread, wmQuit, 0, 0)
	}
}

// acquireSingleInstance 全局命名 mutex 单实例；已有实例返回 false。
// 旧版按「标题+类名」FindWindow 探测，会把用户手开的浏览器窗口误判进来（测试干扰坑），
// 命名 mutex 无此问题。
func acquireSingleInstance() (release func(), ok bool) {
	name := utf16PtrW(`Local\YundianChuanhuatong.SingleInstance`)
	h, _, err := procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(name)))
	if h == 0 {
		logf("创建单实例 mutex 失败：%v", err)
		return func() {}, true // 拿不到锁不拦启动（保守：宁可双开也不打不开）
	}
	if err == syscall.ERROR_ALREADY_EXISTS {
		return func() { procCloseHandle.Call(h) }, false
	}
	return func() { procReleaseMutex.Call(h); procCloseHandle.Call(h) }, true
}

// --- Win32 声明（壳专属；与 tray/window 共用的在各自文件声明） ---
var (
	procPostQuitMsgW       = user32.NewProc("PostQuitMessage")
	procPostThreadMessageW = user32.NewProc("PostThreadMessageW")
	procGetCurrentThreadId = kernel32.NewProc("GetCurrentThreadId")
	procGetModuleHandleW   = kernel32.NewProc("GetModuleHandleW")
	procCreateMutexW       = kernel32.NewProc("CreateMutexW")
	procReleaseMutex       = kernel32.NewProc("ReleaseMutex")
	procDestroyWindow      = user32.NewProc("DestroyWindow")
	procGetWindowRect      = user32.NewProc("GetWindowRect")
)
