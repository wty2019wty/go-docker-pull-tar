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

// Quiet 返回是否处于静默模式。
func (u *UI) Quiet() bool { return u.quiet }

// Verbose 返回是否开启详细日志。
func (u *UI) Verbose() bool { return u.verbose }

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
	if len(line) < u.lastLen {
		pad = strings.Repeat(" ", u.lastLen-len(line))
	}
	fmt.Fprintf(os.Stdout, "\r%s%s", line, pad)
	u.lastLen = len(line)
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
