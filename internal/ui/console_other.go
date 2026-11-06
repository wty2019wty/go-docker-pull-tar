//go:build !windows

package ui

// EnableUTF8Console 在非 Windows 平台无需处理，返回空还原函数。
func EnableUTF8Console() func() {
	return func() {}
}
