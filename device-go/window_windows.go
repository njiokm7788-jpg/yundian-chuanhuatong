// 云点传话筒 · 教室端 —— 开机自启 + 杂项（v2.2.0 起窗口/壳逻辑移入 shell_windows.go）
//
// v2.2.0 迁移说明：本文件原来的 Edge --app 壳管理（launchShell/findEdge/closeStaleShell/
// watchShellGone/closeShellForQuit 按pid杀树/按标题FindWindow/屏幕外预建）已整体删除，
// 由内嵌 WebView2 壳替代——窗口就是本进程自己的 HWND，不再有跨进程生命周期竞态。
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var (
	user32  = syscall.NewLazyDLL("user32.dll")
	shell32 = syscall.NewLazyDLL("shell32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	procShellExecuteW = shell32.NewProc("ShellExecuteW")
	procPostMessageW  = user32.NewProc("PostMessageW")
	// shell_windows.go / closedlg 共用的基础 proc
	procShowWindow          = user32.NewProc("ShowWindow")
	procIsIconic            = user32.NewProc("IsIconic")
	procIsWindowVisible     = user32.NewProc("IsWindowVisible")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procCloseHandle         = kernel32.NewProc("CloseHandle")
)

// hideWindow 给 Windows 子进程用：不弹黑框（powershell TTS 合成等）
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}

// bringExistingToFront 已有实例在跑 → 叫出它的窗口，返回 true。
// 探测方式 = 本机 HTTP /health（沿用 v2.1.x 口径，不依赖窗口类名，不受其他程序窗口干扰）
func bringExistingToFront() bool {
	bp := basePort()
	for p := bp; p < bp+12; p++ {
		url := fmt.Sprintf("http://127.0.0.1:%d/health", p)
		cli := &http.Client{Timeout: 800 * time.Millisecond}
		resp, err := cli.Get(url)
		if err != nil {
			continue
		}
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		_ = resp.Body.Close()
		if fmt.Sprintf("%v", m["app"]) != appName {
			continue
		}
		req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/rpc/show_window", p),
			strings.NewReader(`{"args":[]}`))
		req.Header.Set("Content-Type", "application/json")
		if resp2, err2 := cli.Do(req); err2 == nil {
			_ = resp2.Body.Close()
		}
		return true
	}
	return false
}

const (
	wmClose = 0x0010
	swShow  = 5
)

const (
	swpNoZOrder = 0x0004
	smCXScreen  = 0
	smCYScreen  = 1
)

// rectT 窗口矩形（closedlg 删除后由本文件/壳共用）
type rectT struct{ Left, Top, Right, Bottom int32 }

var (
	procGetSystemMetrics = user32.NewProc("GetSystemMetrics")
	procSetWindowPos     = user32.NewProc("SetWindowPos")
)

func utf16Ptr(s string) *uint16 {
	p, _ := syscall.UTF16PtrFromString(s)
	return p
}

// openFile 用系统默认程序打开文件/目录
func openFile(path string) bool {
	if _, err := os.Stat(path); err != nil {
		logf("打开失败，文件不存在：%s", path)
		return false
	}
	r, _, _ := procShellExecuteW.Call(0, uintptr(unsafe.Pointer(utf16Ptr("open"))),
		uintptr(unsafe.Pointer(utf16Ptr(path))), 0, 0, 1)
	if r <= 32 {
		logf("打开失败：%s", path)
		return false
	}
	return true
}

// openURL 用默认浏览器打开链接（只允许 http/https，防止被注入任意路径/命令）
func openURL(u string) bool {
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		logf("拒绝打开非 http(s) 链接：%s", u)
		return false
	}
	r, _, _ := procShellExecuteW.Call(0, uintptr(unsafe.Pointer(utf16Ptr("open"))),
		uintptr(unsafe.Pointer(utf16Ptr(u))), 0, 0, 1)
	if r <= 32 {
		logf("打开链接失败：%s", u)
		return false
	}
	return true
}

// ---------------------------------------------------------------- 开机自启

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

func autostartOn() bool {
	key, e := regOpen(runKey)
	if e != nil {
		return false
	}
	defer syscall.RegCloseKey(key)
	v, _, e := regQuery(key, "YundianChuanhuatong")
	return e == nil && v != ""
}

// migrateLegacyAutostart 旧名（Chuanhuatong）时代的自启项：改名后老机器上仍指向旧值名/旧 exe 路径，
// 启动时按原意迁到新值名，否则老用户的开机自启会静默失效。
func migrateLegacyAutostart() {
	key, err := regOpen(runKey)
	if err != nil {
		return
	}
	v, _, e := regQuery(key, "Chuanhuatong")
	syscall.RegCloseKey(key)
	if e != nil || v == "" {
		return
	}
	k2, err2 := regCreate(runKey)
	if err2 != nil {
		return
	}
	defer syscall.RegCloseKey(k2)
	if self, err3 := os.Executable(); err3 == nil {
		_ = regSetString(k2, "YundianChuanhuatong", fmt.Sprintf("\"%s\" --silent", self))
	}
	_ = regDeleteValue(k2, "Chuanhuatong")
	logf("开机自启项已从旧名 Chuanhuatong 迁移到 YundianChuanhuatong")
}

func setAutostart(on bool) bool {
	key, err := regCreate(runKey)
	if err != nil {
		logf("开机自启设置失败：%v", err)
		return false
	}
	defer syscall.RegCloseKey(key)
	if !on {
		_ = regDeleteValue(key, "YundianChuanhuatong")
		logf("已取消开机自启")
		return true
	}
	// 优先注册“自己这个 exe”（用户点哪个就注册哪个，跟随发布名：yundianchuanhuatong.exe / 云点传话筒.exe）
	exe := ""
	if self, err2 := os.Executable(); err2 == nil {
		exe = self
	}
	if exe == "" {
		exe = filepath.Join(exeDir, "yundianchuanhuatong.exe")
	}
	val := fmt.Sprintf("\"%s\" --silent", exe)
	if err := regSetString(key, "YundianChuanhuatong", val); err != nil {
		logf("开机自启写入失败：%v", err)
		return false
	}
	logf("已设置开机自启：%s", val)
	return true
}

func regOpen(path string) (syscall.Handle, error) {
	var h syscall.Handle
	err := syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, syscall.StringToUTF16Ptr(path), 0,
		syscall.KEY_READ, &h)
	return h, err
}

// Go 1.26 的 syscall 里没有 RegCreateKeyEx/RegSetValueEx/RegDeleteValue，直接调 advapi32
var (
	advapi32            = syscall.NewLazyDLL("advapi32.dll")
	procRegCreateKeyExW = advapi32.NewProc("RegCreateKeyExW")
	procRegSetValueExW  = advapi32.NewProc("RegSetValueExW")
	procRegDeleteValueW = advapi32.NewProc("RegDeleteValueW")
)

func regCreate(path string) (syscall.Handle, error) {
	var h syscall.Handle
	var disp uint32
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	r, _, _ := procRegCreateKeyExW.Call(uintptr(syscall.HKEY_CURRENT_USER),
		uintptr(unsafe.Pointer(p)), 0, 0, 0,
		uintptr(syscall.KEY_WRITE), 0,
		uintptr(unsafe.Pointer(&h)), uintptr(unsafe.Pointer(&disp)))
	if r != 0 {
		return 0, syscall.Errno(r)
	}
	return h, nil
}

func regSetString(key syscall.Handle, name, val string) error {
	pn, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	d, err := syscall.UTF16FromString(val)
	if err != nil {
		return err
	}
	r, _, _ := procRegSetValueExW.Call(uintptr(key), uintptr(unsafe.Pointer(pn)), 0,
		uintptr(syscall.REG_SZ), uintptr(unsafe.Pointer(&d[0])), uintptr(len(d)*2))
	if r != 0 {
		return syscall.Errno(r)
	}
	return nil
}

func regDeleteValue(key syscall.Handle, name string) error {
	pn, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	r, _, _ := procRegDeleteValueW.Call(uintptr(key), uintptr(unsafe.Pointer(pn)))
	if r != 0 {
		return syscall.Errno(r)
	}
	return nil
}

func regQuery(key syscall.Handle, name string) (string, uint32, error) {
	var typ uint32
	buf := make([]uint16, 512)
	n := uint32(len(buf) * 2)
	if err := syscall.RegQueryValueEx(key, syscall.StringToUTF16Ptr(name), nil, &typ,
		(*byte)(unsafe.Pointer(&buf[0])), &n); err != nil {
		return "", 0, err
	}
	return syscall.UTF16ToString(buf), typ, nil
}
