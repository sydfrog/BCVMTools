//go:build windows

package main

import (
	"os"
	"syscall"
)

// The exe is built as a GUI-subsystem binary (no console flash on double-click).
// When launched from a terminal with arguments, attach to the parent console so
// CLI output and the password prompt still work.
func attachConsole() {
	if len(os.Args) == 1 {
		return
	}
	k := syscall.NewLazyDLL("kernel32.dll")
	if r, _, _ := k.NewProc("AttachConsole").Call(^uintptr(0)); r == 0 {
		return
	}
	if _, err := os.Stdout.Stat(); err != nil {
		if f, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
			os.Stdout = f
		}
	}
	if _, err := os.Stderr.Stat(); err != nil {
		if f, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
			os.Stderr = f
		}
	}
	if _, err := os.Stdin.Stat(); err != nil {
		if f, err := os.OpenFile("CONIN$", os.O_RDWR, 0); err == nil {
			os.Stdin = f
		}
	}
}
