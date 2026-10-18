package main

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// AudioStore 音频缓冲：id -> wav bytes（对应 Python 的 WebApp.audio）
type AudioStore struct {
	mu sync.Mutex
	m  map[int][]byte
}

func NewAudioStore() *AudioStore { return &AudioStore{m: map[int][]byte{}} }

func (s *AudioStore) Get(mid int) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[mid]
}

func (s *AudioStore) Put(mid int, b []byte) {
	s.mu.Lock()
	s.m[mid] = b
	for k := range s.m { // 只留最近几条
		if k < mid-3 {
			delete(s.m, k)
		}
	}
	s.mu.Unlock()
}

func cacheFiles(mid int, text string) (string, string) {
	cached := filepath.Join(cacheDir, itoa(mid)+".wav")
	h := ""
	if text != "" {
		h = filepath.Join(cacheDir, "h_"+textHash(text)+".wav")
	}
	return cached, h
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func readCache(mid int, text string) []byte {
	cached, h := cacheFiles(mid, text)
	for _, f := range []string{h, cached} {
		if f == "" {
			continue
		}
		if fi, err := os.Stat(f); err == nil && fi.Size() > 1024 {
			if b, err := os.ReadFile(f); err == nil {
				return b
			}
		}
	}
	return nil
}

func writeCache(mid int, text string, data []byte) {
	cached, h := cacheFiles(mid, text)
	for _, f := range []string{cached, h} {
		if f == "" {
			continue
		}
		_ = os.WriteFile(f, data, 0o644)
	}
}

const ttsPS = `Add-Type -AssemblyName System.Speech
$ErrorActionPreference='Stop'
$t=[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('__B64__'))
$s=New-Object System.Speech.Synthesis.SpeechSynthesizer
$v=$s.GetInstalledVoices() | Where-Object { $_.VoiceInfo.Culture.Name -like 'zh*' } | Select-Object -First 1
if ($v) { $s.SelectVoice($v.VoiceInfo.Name) }
$s.Rate=0
$s.SetOutputToWaveFile('__OUT__')
$s.Speak($t)
$s.Dispose()
`

// synthLocal 用系统 SAPI（System.Speech）本机合成中文语音，秒级出结果
func synthLocal(text string) []byte {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	out := filepath.Join(cacheDir, "tts_tmp.wav")
	_ = os.Remove(out)
	b64 := base64.StdEncoding.EncodeToString([]byte(text))
	script := strings.Replace(ttsPS, "__B64__", b64, 1)
	script = strings.Replace(script, "__OUT__", strings.ReplaceAll(out, `\`, `\\`), 1)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive",
		"-ExecutionPolicy", "Bypass", "-Command", script)
	hideWindow(cmd)
	if err := cmd.Run(); err != nil {
		logf("本机合成失败：%v", err)
		return nil
	}
	b, err := os.ReadFile(out)
	if err != nil || len(b) < 1024 {
		return nil
	}
	_ = os.Remove(out)
	return b
}
