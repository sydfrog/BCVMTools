//go:build windows

package main

import (
	"fmt"
	"strings"
	"sync"
	"syscall"
	"time"
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

// headerHooks connects a table's native header to the GUI.
type headerHooks struct {
	onDropdown func(col int)                     // arrow clicked
	onContext  func(col int, x, y int32)         // right-click (screen coords)
	style      func(col int) (string, bool, int) // heading text, filtered?, sort (0 none, 1 asc, 2 desc)
}

type subclass struct {
	old    uintptr
	header win.HWND
	headerHooks
	font win.HFONT
}

var (
	user32       = syscall.NewLazyDLL("user32.dll")
	gdi32        = syscall.NewLazyDLL("gdi32.dll")
	pAppendMenu  = user32.NewProc("AppendMenuW")
	pFillRect    = user32.NewProc("FillRect")
	pCreateBrush = gdi32.NewProc("CreateSolidBrush")
	pPolygon     = gdi32.NewProc("Polygon")
	pCreatePen   = gdi32.NewProc("CreatePen")
)

const (
	hhtOnHeader = 0x2
	dtEndEllip  = 0x8000
)

func rgb(r, g, b byte) uintptr { return uintptr(r) | uintptr(g)<<8 | uintptr(b)<<16 }

func fillRect(hdc win.HDC, rc *win.RECT, color uintptr) {
	br, _, _ := pCreateBrush.Call(color)
	pFillRect.Call(uintptr(hdc), uintptr(unsafe.Pointer(rc)), br)
	win.DeleteObject(win.HGDIOBJ(br))
}

func triangle(hdc win.HDC, color uintptr, p [3]win.POINT) {
	br, _, _ := pCreateBrush.Call(color)
	pen, _, _ := pCreatePen.Call(0, 1, color)
	oldB := win.SelectObject(hdc, win.HGDIOBJ(br))
	oldP := win.SelectObject(hdc, win.HGDIOBJ(pen))
	pPolygon.Call(uintptr(hdc), uintptr(unsafe.Pointer(&p[0])), 3)
	win.SelectObject(hdc, oldP)
	win.SelectObject(hdc, oldB)
	win.DeleteObject(win.HGDIOBJ(br))
	win.DeleteObject(win.HGDIOBJ(pen))
}

// drawItem paints one heading: dark grey (amber when the column is filtered), bold text,
// a sort triangle and the filter drop-down triangle.
func (s *subclass) drawItem(cd *win.NMCUSTOMDRAW) {
	col := int(cd.DwItemSpec)
	text, filtered, sortDir := s.style(col)
	dpi := int32(win.GetDpiForWindow(s.header))
	if dpi == 0 {
		dpi = 96
	}
	sc := func(v int32) int32 { return v * dpi / 96 }
	hdc, rc := cd.Hdc, cd.Rc
	bg, fg := rgb(226, 232, 240), rgb(28, 38, 54) // light blue-grey with dark bold text
	if filtered {
		bg, fg = rgb(255, 214, 110), rgb(28, 28, 28)
	}
	if cd.UItemState&1 != 0 { // pressed
		if filtered {
			bg = rgb(245, 198, 80)
		} else {
			bg = rgb(205, 214, 226)
		}
	}
	fillRect(hdc, &rc, bg)
	fillRect(hdc, &win.RECT{Left: rc.Right - 1, Top: rc.Top, Right: rc.Right, Bottom: rc.Bottom}, rgb(176, 186, 200))

	if s.font == 0 {
		var lf win.LOGFONT
		lf.LfHeight = -(9 * dpi / 72)
		lf.LfWeight = win.FW_BOLD
		lf.LfQuality = 5 // CLEARTYPE_QUALITY
		copy(lf.LfFaceName[:], syscall.StringToUTF16("Segoe UI"))
		s.font = win.CreateFontIndirect(&lf)
	}
	old := win.SelectObject(hdc, win.HGDIOBJ(s.font))
	win.SetBkMode(hdc, win.TRANSPARENT)
	win.SetTextColor(hdc, win.COLORREF(fg))
	tr := win.RECT{Left: rc.Left + sc(8), Top: rc.Top, Right: rc.Right - sc(38), Bottom: rc.Bottom}
	if txt, err := syscall.UTF16PtrFromString(text); err == nil {
		win.DrawTextEx(hdc, txt, -1, &tr, win.DT_SINGLELINE|win.DT_VCENTER|dtEndEllip, nil)
	}
	win.SelectObject(hdc, old)

	cy := (rc.Top + rc.Bottom) / 2
	ax := rc.Right - sc(12)
	triangle(hdc, fg, [3]win.POINT{{X: ax - sc(4), Y: cy - sc(2)}, {X: ax + sc(4), Y: cy - sc(2)}, {X: ax, Y: cy + sc(3)}})
	sx := rc.Right - sc(30)
	switch sortDir {
	case 1:
		triangle(hdc, fg, [3]win.POINT{{X: sx - sc(4), Y: cy + sc(2)}, {X: sx + sc(4), Y: cy + sc(2)}, {X: sx, Y: cy - sc(3)}})
	case 2:
		triangle(hdc, fg, [3]win.POINT{{X: sx - sc(4), Y: cy - sc(2)}, {X: sx + sc(4), Y: cy - sc(2)}, {X: sx, Y: cy + sc(3)}})
	}
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
		if lp == 0 {
			break
		}
		ptr := *(*unsafe.Pointer)(unsafe.Pointer(&lp))
		nm := (*win.NMHDR)(ptr)
		if nm.HwndFrom != s.header {
			break
		}
		switch nm.Code {
		case win.HDN_DROPDOWN:
			s.onDropdown(int((*nmHeader)(ptr).Item))
			return 0
		case win.NM_RCLICK:
			var pt win.POINT
			win.GetCursorPos(&pt)
			cp := pt
			win.ScreenToClient(s.header, &cp)
			info := win.HDHITTESTINFO{Pt: cp}
			win.SendMessage(s.header, win.HDM_HITTEST, 0, uintptr(unsafe.Pointer(&info)))
			if info.IItem >= 0 && info.Flags&hhtOnHeader != 0 {
				s.onContext(int(info.IItem), pt.X, pt.Y)
			}
			return 0
		case win.NM_CUSTOMDRAW:
			cd := (*win.NMCUSTOMDRAW)(ptr)
			switch cd.DwDrawStage {
			case win.CDDS_PREPAINT:
				return win.CDRF_NOTIFYITEMDRAW
			case win.CDDS_ITEMPREPAINT:
				s.drawItem(cd)
				return win.CDRF_SKIPDEFAULT
			}
			return win.CDRF_DODEFAULT
		}
	case win.WM_NCDESTROY:
		subMu.Lock()
		delete(subs, h)
		subMu.Unlock()
	}
	return win.CallWindowProc(s.old, h, uint32(msg), wp, lp)
}

// listViewOf finds the real list view inside a walk TableView. The TableView is a
// container holding a frozen and a normal SysListView32; the normal one has the columns.
func listViewOf(tv *walk.TableView) (lv, hdr win.HWND) {
	best := int32(-1)
	for c := win.GetWindow(tv.Handle(), win.GW_CHILD); c != 0; c = win.GetWindow(c, win.GW_HWNDNEXT) {
		var buf [64]uint16
		n, _ := win.GetClassName(c, &buf[0], len(buf))
		if syscall.UTF16ToString(buf[:n]) != "SysListView32" {
			continue
		}
		h := win.HWND(win.SendMessage(c, win.LVM_GETHEADER, 0, 0))
		if h == 0 {
			continue
		}
		if cnt := int32(win.SendMessage(h, win.HDM_GETITEMCOUNT, 0, 0)); cnt > best {
			best, lv, hdr = cnt, c, h
		}
	}
	return
}

func headerOf(tv *walk.TableView) win.HWND {
	_, h := listViewOf(tv)
	return h
}

// hookHeader subclasses the table so its header is custom drawn and reports arrow / right clicks.
func hookHeader(tv *walk.TableView, hk headerHooks) {
	if safeUI {
		return
	}
	h, hdr := listViewOf(tv)
	if h == 0 {
		return
	}
	old := win.SetWindowLongPtr(h, win.GWLP_WNDPROC, subProc)
	if old == 0 {
		return
	}
	subMu.Lock()
	subs[h] = &subclass{old: old, header: hdr, headerHooks: hk}
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

func repaintHeader(tv *walk.TableView) {
	if hdr := headerOf(tv); hdr != 0 {
		win.InvalidateRect(hdr, nil, true)
	}
}

// headerMenu pops up the right-click menu for a heading and returns the chosen command
// (1 filter, 2 clear this column, 3 sort A-Z, 4 sort Z-A, 5 clear all filters; 0 none).
func headerMenu(owner win.HWND, x, y int32, hasFilter bool) int {
	hm := win.CreatePopupMenu()
	if hm == 0 {
		return 0
	}
	defer win.DestroyMenu(hm)
	add := func(flags uintptr, id uintptr, text string) {
		p, _ := syscall.UTF16PtrFromString(text)
		pAppendMenu.Call(uintptr(hm), flags, id, uintptr(unsafe.Pointer(p)))
	}
	add(win.MF_STRING, 1, "Filter...")
	clearFlags := uintptr(win.MF_STRING)
	if !hasFilter {
		clearFlags |= win.MF_GRAYED
	}
	add(clearFlags, 2, "Clear filter on this column")
	add(win.MF_SEPARATOR, 0, "")
	add(win.MF_STRING, 3, "Sort A to Z")
	add(win.MF_STRING, 4, "Sort Z to A")
	add(win.MF_SEPARATOR, 0, "")
	add(win.MF_STRING, 5, "Clear all filters on this tab")
	return int(win.TrackPopupMenuEx(hm, win.TPM_RETURNCMD|win.TPM_LEFTALIGN|win.TPM_TOPALIGN, x, y, owner, nil))
}

// columnPickMenu lists all columns (filtered ones marked) at the cursor and returns the chosen index or -1.
func columnPickMenu(owner win.HWND, names []string, filtered []bool) int {
	hm := win.CreatePopupMenu()
	if hm == 0 {
		return -1
	}
	defer win.DestroyMenu(hm)
	for i, n := range names {
		if filtered[i] {
			n = "\u25CF " + n
		}
		p, _ := syscall.UTF16PtrFromString(n)
		pAppendMenu.Call(uintptr(hm), win.MF_STRING, uintptr(i+1), uintptr(unsafe.Pointer(p)))
	}
	var pt win.POINT
	win.GetCursorPos(&pt)
	r := int(win.TrackPopupMenuEx(hm, win.TPM_RETURNCMD|win.TPM_LEFTALIGN|win.TPM_TOPALIGN, pt.X, pt.Y, owner, nil))
	return r - 1
}

// ---- hang diagnostics ------------------------------------------------------------------------

var (
	k32                 = syscall.NewLazyDLL("kernel32.dll")
	ntdll               = syscall.NewLazyDLL("ntdll.dll")
	pOpenThread         = k32.NewProc("OpenThread")
	pSuspendThread      = k32.NewProc("SuspendThread")
	pResumeThread       = k32.NewProc("ResumeThread")
	pGetThreadContext   = k32.NewProc("GetThreadContext")
	pGetThreadTimes     = k32.NewProc("GetThreadTimes")
	pCloseHandle        = k32.NewProc("CloseHandle")
	pGetModuleHandleEx  = k32.NewProc("GetModuleHandleExW")
	pGetModuleFileName  = k32.NewProc("GetModuleFileNameW")
	pGetCurrentThreadID = k32.NewProc("GetCurrentThreadId")
	pGetGuiResources    = user32.NewProc("GetGuiResources")
	pRtlLookupFunction  = ntdll.NewProc("RtlLookupFunctionEntry")
	pRtlVirtualUnwind   = ntdll.NewProc("RtlVirtualUnwind")
	pRtlGetVersion      = ntdll.NewProc("RtlGetVersion")
)

func currentThreadID() uint32 {
	id, _, _ := pGetCurrentThreadID.Call()
	return uint32(id)
}

// sampleStack suspends the given thread for a moment and unwinds its native stack (amd64),
// returning raw instruction pointers. No Go allocation happens while the thread is suspended.
func sampleStack(h uintptr, frames *[24]uintptr, ctxBuf *[1232 + 16]byte) int {
	off := int((16 - uintptr(unsafe.Pointer(&ctxBuf[0]))%16) % 16) // CONTEXT must be 16-byte aligned
	cp := unsafe.Pointer(&ctxBuf[off])
	ctx := uintptr(cp)
	*(*uint32)(unsafe.Add(cp, 0x30)) = 0x100003 // CONTEXT_CONTROL | CONTEXT_INTEGER
	if r, _, _ := pSuspendThread.Call(h); r == ^uintptr(0) {
		return 0
	}
	n := 0
	if r, _, _ := pGetThreadContext.Call(h, ctx); r != 0 {
		for n < len(frames) {
			rip := *(*uint64)(unsafe.Add(cp, 0xF8))
			if rip == 0 {
				break
			}
			frames[n] = uintptr(rip)
			n++
			var imageBase uint64
			fe, _, _ := pRtlLookupFunction.Call(uintptr(rip), uintptr(unsafe.Pointer(&imageBase)), 0)
			if fe == 0 {
				break // no unwind info (Go frames, or the end of the stack)
			}
			var handlerData uintptr
			var establisher uint64
			pRtlVirtualUnwind.Call(0, uintptr(imageBase), uintptr(rip), fe, ctx,
				uintptr(unsafe.Pointer(&handlerData)), uintptr(unsafe.Pointer(&establisher)), 0)
		}
	}
	pResumeThread.Call(h)
	return n
}

func moduleOf(addr uintptr) string {
	var hmod uintptr
	if r, _, _ := pGetModuleHandleEx.Call(6, addr, uintptr(unsafe.Pointer(&hmod))); r == 0 { // FROM_ADDRESS | UNCHANGED_REFCOUNT
		return fmt.Sprintf("0x%x", addr)
	}
	var buf [260]uint16
	pGetModuleFileName.Call(hmod, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	name := syscall.UTF16ToString(buf[:])
	if i := strings.LastIndexAny(name, `\/`); i >= 0 {
		name = name[i+1:]
	}
	return fmt.Sprintf("%s+0x%x", name, addr-hmod)
}

func threadCPU(h uintptr) (ms int64) {
	var c, e, k, u [2]uint32
	if r, _, _ := pGetThreadTimes.Call(h, uintptr(unsafe.Pointer(&c)), uintptr(unsafe.Pointer(&e)), uintptr(unsafe.Pointer(&k)), uintptr(unsafe.Pointer(&u))); r == 0 {
		return -1
	}
	ft := func(v [2]uint32) int64 { return (int64(v[1])<<32 | int64(v[0])) / 10000 }
	return ft(k) + ft(u)
}

// uiHangReport describes what the (hung) UI thread is doing: CPU use over a couple of seconds
// (spinning vs blocked), three native stack samples, and the process's GDI/USER object counts.
func uiHangReport(tid uint32) string {
	var sb strings.Builder
	if tid == 0 {
		return "UI thread id unknown\n"
	}
	h, _, _ := pOpenThread.Call(0x0002|0x0008|0x0040, 0, uintptr(tid)) // SUSPEND_RESUME | GET_CONTEXT | QUERY_INFORMATION
	if h == 0 {
		return "could not open the UI thread\n"
	}
	defer pCloseHandle.Call(h)
	proc, _, _ := syscall.NewLazyDLL("kernel32.dll").NewProc("GetCurrentProcess").Call()
	gdi, _, _ := pGetGuiResources.Call(proc, 0)
	usr, _, _ := pGetGuiResources.Call(proc, 1)
	var ver [5]uint32 // OSVERSIONINFOW head: size, major, minor, build, platform
	ver[0] = 276
	vbuf := make([]byte, 276)
	*(*uint32)(unsafe.Pointer(&vbuf[0])) = 276
	pRtlGetVersion.Call(uintptr(unsafe.Pointer(&vbuf[0])))
	fmt.Fprintf(&sb, "windows %d.%d build %d; GDI objects %d, USER objects %d (limit about 10000)\n",
		*(*uint32)(unsafe.Pointer(&vbuf[4])), *(*uint32)(unsafe.Pointer(&vbuf[8])), *(*uint32)(unsafe.Pointer(&vbuf[12])), gdi, usr)

	cpu0 := threadCPU(h)
	var ctx [1232 + 16]byte
	for s := 0; s < 3; s++ {
		var frames [24]uintptr
		n := sampleStack(h, &frames, &ctx)
		fmt.Fprintf(&sb, "native stack sample %d (%d frames):\n", s+1, n)
		for i := 0; i < n; i++ {
			fmt.Fprintf(&sb, "    %s\n", moduleOf(frames[i]))
		}
		time.Sleep(time.Second)
	}
	cpu1 := threadCPU(h)
	fmt.Fprintf(&sb, "UI thread CPU time used during the 3s sampling: %d ms (about 3000 = spinning, about 0 = blocked/waiting)\n", cpu1-cpu0)
	return sb.String()
}
