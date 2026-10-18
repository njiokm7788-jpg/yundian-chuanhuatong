package main

// 本机音频播放（winmm PlaySound，内存直接播 WAV，不落盘）
// 用途：课堂机网页端播放被拦（无音频设备/自动播放策略/取音失败）时的兜底，
// 保证教室一定出声——这是"喊话"产品的核心动作。

import (
	"sync"
	"syscall"
	"unsafe"
)

var (
	winmmDLL      = syscall.NewLazyDLL("winmm.dll")
	procPlaySound = winmmDLL.NewProc("PlaySoundW")
)

const (
	sndAsync     = 0x0001 // 异步播放，立刻返回
	sndNoDefault = 0x0002 // 资源不存在时不要放系统默认提示音
	sndMemory    = 0x0004 // pszSound 指向内存中的 WAV 数据
)

var (
	playMu   sync.Mutex
	playBuf  []byte // 异步播放期间必须保持缓冲存活，否则 GC 回收会崩
	playBusy bool
)

// playWav 播放一段 WAV（内存）。返回是否成功起播。
func playWav(data []byte) bool {
	if len(data) < 44 { // 最小 WAV 头
		return false
	}
	playMu.Lock()
	playBuf = data
	playBusy = true
	playMu.Unlock()
	r, _, _ := procPlaySound.Call(uintptr(unsafe.Pointer(&data[0])), 0,
		uintptr(sndMemory|sndAsync|sndNoDefault))
	ok := r != 0
	playMu.Lock()
	playBusy = ok
	if !ok {
		playBuf = nil
	}
	playMu.Unlock()
	return ok
}

// stopWav 停止当前播放（PlaySound(NULL,0,0)）
func stopWav() {
	playMu.Lock()
	playBuf = nil
	playBusy = false
	playMu.Unlock()
	procPlaySound.Call(0, 0, 0)
}
