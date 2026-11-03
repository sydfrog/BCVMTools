// Copyright 2019 The Walk Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build windows,!walk_use_cgo

package walk

import (
	"unsafe"

	"github.com/lxn/win"
)

func (fb *FormBase) mainLoop() int {
	msg := (*win.MSG)(unsafe.Pointer(win.GlobalAlloc(0, unsafe.Sizeof(win.MSG{}))))
	defer win.GlobalFree(win.HGLOBAL(unsafe.Pointer(msg)))

	for fb.hWnd != 0 {
		switch win.GetMessage(msg, 0, 0, 0) {
		case 0:
			return int(msg.WParam)

		case -1:
			return -1
		}

		switch msg.Message {
		case win.WM_KEYDOWN:
			if fb.handleKeyDown(msg) {
				continue
			}
		}

		recordMsg(uintptr(msg.HWnd), msg.Message, msg.WParam, msg.LParam)

		// BCVMTools: IsDialogMessage only has work to do for keyboard messages, but the stock loop
		// called it for every message. On some Windows 11 builds it was seen spinning inside
		// user32 (UI hang with no input needed), so it is now only used for keyboard input.
		isKey := false
		switch msg.Message {
		case win.WM_KEYDOWN, win.WM_KEYUP, win.WM_CHAR, win.WM_DEADCHAR, win.WM_SYSKEYDOWN, win.WM_SYSKEYUP, win.WM_SYSCHAR, win.WM_SYSDEADCHAR:
			isKey = true
		}
		if !isKey || !win.IsDialogMessage(fb.hWnd, msg) {
			win.TranslateMessage(msg)
			win.DispatchMessage(msg)
		}

		fb.group.RunSynchronized()
	}

	return 0
}
