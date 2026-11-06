//go:build windows

package ui

import "syscall"

// kernel32 控制台代码页相关 API（仅使用标准库，保持零第三方依赖）。
var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleCP       = kernel32.NewProc("GetConsoleCP")
	procSetConsoleCP       = kernel32.NewProc("SetConsoleCP")
	procGetConsoleOutputCP = kernel32.NewProc("GetConsoleOutputCP")
	procSetConsoleOutputCP = kernel32.NewProc("SetConsoleOutputCP")
)

const codePageUTF8 = 65001

// EnableUTF8Console 将 Windows 控制台的输入/输出代码页切换为 UTF-8（65001），
// 避免 GBK 等传统代码页下中文输出乱码。返回的函数用于退出时恢复原代码页，
// 不影响父 shell；进程未附加控制台（输出被重定向）时调用无副作用。
func EnableUTF8Console() func() {
	oldIn, _, _ := procGetConsoleCP.Call()
	oldOut, _, _ := procGetConsoleOutputCP.Call()
	procSetConsoleCP.Call(codePageUTF8)
	procSetConsoleOutputCP.Call(codePageUTF8)
	return func() {
		if oldIn != 0 {
			procSetConsoleCP.Call(oldIn)
		}
		if oldOut != 0 {
			procSetConsoleOutputCP.Call(oldOut)
		}
	}
}
