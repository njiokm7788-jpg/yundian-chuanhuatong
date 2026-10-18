// 云点传话筒 · 教室端 —— 系统托盘（Windows 原生 Shell_NotifyIcon，无第三方依赖）
//
// 左键单击/双击托盘图标 → 打开界面；右键 → 菜单（打开界面 / 隐藏到托盘 / 关闭程序）
// 关窗（点 ×）后的选择框逻辑见 close_flow_windows.go
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

const (
	classTray = "YundianChuanhuatongTray"

	wmApp             = 0x8000
	wmTray            = wmApp + 1
	wmLButtonUp       = 0x0202
	wmLButtonDblClk   = 0x0203
	wmRButtonUp       = 0x0205
	wmContextMenu     = 0x007B
	wmCommand         = 0x0111
	wmDestroy         = 0x0002
	wmQueryEndSession = 0x0011
	wmNull            = 0x0000

	nimAdd     = 0
	nimModify  = 1
	nimDelete  = 2
	nifMessage = 0x0001
	nifIcon    = 0x0002
	nifTip     = 0x0004
	nifInfo    = 0x0010

	tpmRightButton = 0x0002
	tpmReturnCmd   = 0x0100

	menuOpen = 1
	menuHide = 2
	menuQuit = 3

	idiApplication        = 32512
	errClassAlreadyExists = 1410
)

var (
	kernelTray           = syscall.NewLazyDLL("kernel32.dll")

	procRegisterClassExW = user32.NewProc("RegisterClassExW")
	procCreateWindowExW  = user32.NewProc("CreateWindowExW")
	procDefWindowProcW   = user32.NewProc("DefWindowProcW")
	procGetMessageW      = user32.NewProc("GetMessageW")
	procTranslateMessage = user32.NewProc("TranslateMessage")
	procDispatchMessageW = user32.NewProc("DispatchMessageW")
	procPostQuitMessage  = user32.NewProc("PostQuitMessage")
	procLoadIconW        = user32.NewProc("LoadIconW")
	procLoadImageW       = user32.NewProc("LoadImageW")
	procCreatePopupMenu  = user32.NewProc("CreatePopupMenu")
	procAppendMenuW      = user32.NewProc("AppendMenuW")
	procTrackPopupMenu   = user32.NewProc("TrackPopupMenu")
	procDestroyMenu      = user32.NewProc("DestroyMenu")
	procGetCursorPos     = user32.NewProc("GetCursorPos")
	procShellNotifyIconW = shell32.NewProc("Shell_NotifyIconW")
	procExtractIconW     = shell32.NewProc("ExtractIconW")
)

type trayPointT struct{ x, y int32 }

type trayMsgT struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      trayPointT
}

type trayWndClassExW struct {
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

type trayNidW struct {
	cbSize           uint32
	hWnd             uintptr
	uID              uint32
	uFlags           uint32
	uCallbackMessage uint32
	hIcon            uintptr
	szTip            [128]uint16
	dwState          uint32
	dwStateMask      uint32
	szInfo           [256]uint16
	uTimeout         uint32
	szInfoTitle      [64]uint16
	dwInfoFlags      uint32
	guidItem         [16]byte
	hBalloonIcon     uintptr
}

var (
	trayHwnd uintptr
	trayData trayNidW
)

func copyUTF16(dst []uint16, s string) {
	u := utf16.Encode([]rune(s))
	n := copy(dst, u)
	if n < len(dst) {
		dst[n] = 0
	} else {
		dst[len(dst)-1] = 0
	}
}

// startTray 起托盘（独立线程 + 消息循环）
func startTray() {
	migrateLegacyAutostart() // 旧名（Chuanhuatong）自启项一次性迁移
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := trayCreate(); err != nil {
			logf("托盘图标创建失败：%v", err)
			return
		}
		var msg trayMsgT
		for {
			r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
			if int32(r) <= 0 {
				return
			}
			procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
			procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
		}
	}()
}

func trayCreate() error {
	hInst, _, _ := procGetModuleHandleW.Call(0)
	wc := trayWndClassExW{
		cbSize:        uint32(unsafe.Sizeof(trayWndClassExW{})),
		lpfnWndProc:   syscall.NewCallback(trayProc),
		hInstance:     hInst,
		lpszClassName: utf16Ptr(classTray),
	}
	if r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		if e, ok := err.(syscall.Errno); !ok || uintptr(e) != errClassAlreadyExists {
			return fmt.Errorf("RegisterClassExW 失败：%v", err)
		}
	}
	hwnd, _, err := procCreateWindowExW.Call(0,
		uintptr(unsafe.Pointer(utf16Ptr(classTray))),
		uintptr(unsafe.Pointer(utf16Ptr(appName))),
		0x80000000 /*WS_POPUP*/, 0, 0, 0, 0, 0, 0, hInst, 0)
	if hwnd == 0 {
		return fmt.Errorf("CreateWindowExW 失败：%v", err)
	}
	trayHwnd = hwnd

	trayData = trayNidW{
		cbSize:           uint32(unsafe.Sizeof(trayNidW{})),
		hWnd:             hwnd,
		uID:              1,
		uFlags:           nifMessage | nifIcon | nifTip,
		uCallbackMessage: wmTray,
		hIcon:            trayLoadIcon(hInst),
	}
	copyUTF16(trayData.szTip[:], appName+"：双击打开界面")

	var err2 error
	for i := 0; i < 5; i++ {
		r, _, e := procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&trayData)))
		if r != 0 {
			logf("托盘图标已就绪（双击托盘可打开界面）")
			return nil
		}
		err2 = e
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("Shell_NotifyIcon 失败：%v", err2)
}

// trayLoadIcon exe 自带图标 → 界面 favicon → 系统默认图标
func trayLoadIcon(hInst uintptr) uintptr {
	if exe, err := os.Executable(); err == nil {
		if h, _, _ := procExtractIconW.Call(hInst, uintptr(unsafe.Pointer(utf16Ptr(exe))), 0); h > 1 {
			return h
		}
	}
	if b, err := uiEmbed.ReadFile("ui/favicon.ico"); err == nil {
		p := filepath.Join(os.TempDir(), "yundianchuanhuatong_tray.ico")
		if os.WriteFile(p, b, 0o644) == nil {
			h, _, _ := procLoadImageW.Call(0, uintptr(unsafe.Pointer(utf16Ptr(p))),
				1 /*IMAGE_ICON*/, 0, 0, 0x0010 /*LR_LOADFROMFILE*/)
			if h != 0 {
				return h
			}
		}
	}
	h, _, _ := procLoadIconW.Call(0, idiApplication)
	return h
}

func trayRemove() {
	if trayHwnd == 0 {
		return
	}
	procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&trayData)))
	trayHwnd = 0
}

// trayNotify 托盘气泡提示（界面关掉后仍能告知用户程序还在运行）
func trayNotify(title, text string) {
	if trayHwnd == 0 {
		return
	}
	d := trayData
	d.uFlags = nifInfo
	d.uTimeout = 5000
	d.dwInfoFlags = 1 /*NIIF_INFO*/
	copyUTF16(d.szInfoTitle[:], title)
	copyUTF16(d.szInfo[:], text)
	procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&d)))
}

func trayProc(hwnd, msg, wparam, lparam uintptr) uintptr {
	switch msg {
	case wmTray:
		switch lparam & 0xFFFF {
		case wmLButtonUp, wmLButtonDblClk:
			go showShellWindow("tray-click")
		case wmRButtonUp, wmContextMenu:
			trayMenu(hwnd)
		}
		return 0
	case wmCommand:
		switch wparam & 0xFFFF {
		case menuOpen:
			go showShellWindow("tray-menu")
		case menuHide:
			go setWinVisible(false)
		case menuQuit:
			go requestCloseExplicit("tray-menu")
		}
		return 0
	case wmQueryEndSession:
		return 1
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
	return r
}

func trayMenu(hwnd uintptr) {
	hMenu, _, _ := procCreatePopupMenu.Call()
	if hMenu == 0 {
		return
	}
	defer procDestroyMenu.Call(hMenu)
	appendItem := func(id int, text string) {
		procAppendMenuW.Call(hMenu, 0, uintptr(id), uintptr(unsafe.Pointer(utf16Ptr(text))))
	}
	appendItem(menuOpen, "打开界面")
	appendItem(menuHide, "隐藏到托盘")
	procAppendMenuW.Call(hMenu, 0x800 /*MF_SEPARATOR*/, 0, 0)
	appendItem(menuQuit, "关闭程序")
	var pt trayPointT
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	procSetForegroundWindow.Call(hwnd)
	cmd, _, _ := procTrackPopupMenu.Call(hMenu, tpmRightButton|tpmReturnCmd,
		uintptr(pt.x), uintptr(pt.y), 0, hwnd, 0)
	procPostMessageW.Call(hwnd, wmNull, 0, 0)
	switch cmd {
	case menuOpen:
		go showShellWindow("tray-menu")
	case menuHide:
		go setWinVisible(false)
	case menuQuit:
		go requestCloseExplicit("tray-menu")
	}
}

var errNoTray = errors.New("托盘未就绪")
