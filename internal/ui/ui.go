// Package ui 提供简洁的终端输出与进度渲染。
package ui

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// UI 负责在进度条与普通日志之间协调输出。
type UI struct {
	mu       sync.Mutex
	quiet    bool
	verbose  bool
	active   bool
	lastLen  int
	disabled bool
}

// New 创建 UI。
func New(quiet, verbose bool) *UI {
	return &UI{quiet: quiet, verbose: verbose}
}

// Infof 输出普通信息（静默模式下不输出）。
func (u *UI) Infof(format string, args ...interface{}) {
	if u.quiet {
		return
	}
	u.Print(format, args...)
}

// Printf 总是输出信息。
func (u *UI) Printf(format string, args ...interface{}) {
	u.Print(format, args...)
}

// Verbosef 仅在详细模式下输出。
func (u *UI) Verbosef(format string, args ...interface{}) {
	if !u.verbose {
		return
	}
	u.Print(format, args...)
}

// Print 清除当前进度行后输出一行日志。
func (u *UI) Print(format string, args ...interface{}) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.clearLocked()
	fmt.Fprintf(os.Stdout, format+"\n", args...)
}

// Errorf 输出错误信息到标准错误。
func (u *UI) Errorf(format string, args ...interface{}) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.clearLocked()
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

// StartStallWatch 启动卡顿监视：当某个阶段迟迟未完成时，每隔 interval 打印一次
// 等待提示，让用户知道程序仍在运行、当前卡在哪一步。
// 返回停止函数，可安全地重复调用。
func (u *UI) StartStallWatch(stage string, interval time.Duration) func() {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	done := make(chan struct{})
	var once sync.Once
	stop := func() {
		once.Do(func() { close(done) })
	}
	start := time.Now()
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				u.Infof("仍在%s...（已等待 %s，可按 Ctrl+C 中断）", stage, time.Since(start).Round(time.Second))
			}
		}
	}()
	return stop
}

// Progress 渲染一行进度（不换行）。
func (u *UI) Progress(line string) {
	if u.quiet {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	pad := ""
	w := displayWidth(line)
	if w < u.lastLen {
		pad = strings.Repeat(" ", u.lastLen-w)
	}
	fmt.Fprintf(os.Stdout, "\r%s%s", line, pad)
	u.lastLen = w
	u.active = true
}

// ClearProgress 清除进度行。
func (u *UI) ClearProgress() {
	if u.quiet {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.clearLocked()
}

func (u *UI) clearLocked() {
	if !u.active {
		return
	}
	fmt.Fprintf(os.Stdout, "\r%s\r", strings.Repeat(" ", u.lastLen))
	u.active = false
	u.lastLen = 0
}

// displayWidth 估算字符串在终端中的显示宽度（东亚宽字符按 2 列计），
// 用于进度行的覆盖填充与清除。
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

// runeWidth 返回单个字符占用的终端列宽。
func runeWidth(r rune) int {
	switch {
	case r >= 0x1100 && r <= 0x115F, // 韩文字母
		r >= 0x2E80 && r <= 0xA4CF, // CJK 部首、假名、汉字
		r >= 0xAC00 && r <= 0xD7A3, // 韩文音节
		r >= 0xF900 && r <= 0xFAFF, // CJK 兼容汉字
		r >= 0xFE30 && r <= 0xFE6F, // CJK 兼容符号
		r >= 0xFF00 && r <= 0xFF60, // 全角字符
		r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x20000: // CJK 扩展
		return 2
	}
	return 1
}
