package view

import (
	"os"
	"syscall"
)

// enableVirtualTerminalProcessing makes a Windows console interpret ANSI
// sequences.
const enableVirtualTerminalProcessing = 0x0004

var procSetConsoleMode = syscall.NewLazyDLL("kernel32.dll").NewProc("SetConsoleMode")

// EnableANSI makes the console behind f interpret ANSI sequences. ok is false
// when it cannot; restore puts the console mode back.
func EnableANSI(f *os.File) (restore func(), ok bool) {
	h := syscall.Handle(f.Fd())
	var mode uint32
	if err := syscall.GetConsoleMode(h, &mode); err != nil {
		return func() {}, false
	}
	if mode&enableVirtualTerminalProcessing != 0 {
		return func() {}, true
	}
	if r, _, _ := procSetConsoleMode.Call(uintptr(h), uintptr(mode|enableVirtualTerminalProcessing)); r == 0 {
		return func() {}, false
	}
	return func() { _, _, _ = procSetConsoleMode.Call(uintptr(h), uintptr(mode)) }, true
}
