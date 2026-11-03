//go:build windows

package main

import (
	"sync"
	"syscall"
	"unsafe"

	"github.com/lxn/walk"
	"github.com/lxn/win"
)

// Excel-style header filter arrows are native header "split buttons"
// (HDF_SPLITBUTTON). Clicking the arrow sends HDN_DROPDOWN to the list view,
// which we intercept by subclassing it; clicking the heading text still sorts.
const hdfSplitButton = 0x1000000

type nmHeader struct {
	win.NMHDR
	Item   int32
	Button int32
	PItem  *win.HDITEM
}

type subclass struct {
	old        uintptr
	header     win.HWND
	onDropdown func(col int)
}

var (
	subMu   sync.Mutex
	subs    = map[win.HWND]*subclass{}
	subProc = syscall.NewCallback(listViewProc)
)

func listViewProc(hwnd, msg, wp, lp uintptr) uintptr {
	h := win.HWND(hwnd)
	subMu.Lock()
	s := subs[h]
	subMu.Unlock()
	if s == nil {
		return win.DefWindowProc(h, uint32(msg), wp, lp)
	}
	switch uint32(msg) {
	case win.WM_NOTIFY:
		if lp != 0 {
			nm := (*nmHeader)(*(*unsafe.Pointer)(unsafe.Pointer(&lp)))
			if nm.Code == win.HDN_DROPDOWN && nm.HwndFrom == s.header {
				s.onDropdown(int(nm.Item))
				return 0
			}
		}
	case win.WM_NCDESTROY:
		subMu.Lock()
		delete(subs, h)
		subMu.Unlock()
	}
	return win.CallWindowProc(s.old, h, uint32(msg), wp, lp)
}

func headerOf(tv *walk.TableView) win.HWND {
	return win.HWND(win.SendMessage(tv.Handle(), win.LVM_GETHEADER, 0, 0))
}

// hookHeaderDropdown routes header drop-down arrow clicks of tv to fn(column).
func hookHeaderDropdown(tv *walk.TableView, fn func(col int)) {
	hdr := headerOf(tv)
	if hdr == 0 {
		return
	}
	h := tv.Handle()
	old := win.SetWindowLongPtr(h, win.GWLP_WNDPROC, subProc)
	if old == 0 {
		return
	}
	subMu.Lock()
	subs[h] = &subclass{old: old, header: hdr, onDropdown: fn}
	subMu.Unlock()
}

// markHeaderButtons turns on the drop-down arrow for the first n header items.
func markHeaderButtons(tv *walk.TableView, n int) {
	hdr := headerOf(tv)
	if hdr == 0 {
		return
	}
	for i := 0; i < n; i++ {
		var it win.HDITEM
		it.Mask = win.HDI_FORMAT
		p := uintptr(unsafe.Pointer(&it))
		if win.SendMessage(hdr, win.HDM_GETITEM, uintptr(i), p) == 0 {
			continue
		}
		it.Fmt |= hdfSplitButton
		win.SendMessage(hdr, win.HDM_SETITEM, uintptr(i), p)
	}
}
