// Package ui 提供简洁的终端输出与进度渲染。
package ui

import (
	"fmt"
	"os"
	"strings"
	"sync"
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
