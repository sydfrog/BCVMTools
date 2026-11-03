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
	oldB := win.SelectObject(hdc, win.HGDIOBJ(br))
	oldP := win.SelectObject(hdc, win.GetStockObject(win.BLACK_PEN))
	pPolygon.Call(uintptr(hdc), uintptr(unsafe.Pointer(&p[0])), 3)
	win.SelectObject(hdc, oldP)
	win.SelectObject(hdc, oldB)
	win.DeleteObject(win.HGDIOBJ(br))
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
	bg, fg := rgb(150, 156, 164), rgb(0, 0, 0)
	if filtered {
		bg = rgb(255, 183, 77)
	}
	if cd.UItemState&1 != 0 { // pressed
		bg = bg - rgb(20, 20, 20)
	}
	fillRect(hdc, &rc, bg)
	fillRect(hdc, &win.RECT{Left: rc.Right - 1, Top: rc.Top, Right: rc.Right, Bottom: rc.Bottom}, rgb(95, 100, 108))

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
	triangle(hdc, rgb(0, 0, 0), [3]win.POINT{{X: ax - sc(4), Y: cy - sc(2)}, {X: ax + sc(4), Y: cy - sc(2)}, {X: ax, Y: cy + sc(3)}})
	sx := rc.Right - sc(30)
	switch sortDir {
	case 1:
		triangle(hdc, rgb(0, 0, 0), [3]win.POINT{{X: sx - sc(4), Y: cy + sc(2)}, {X: sx + sc(4), Y: cy + sc(2)}, {X: sx, Y: cy - sc(3)}})
	case 2:
		triangle(hdc, rgb(0, 0, 0), [3]win.POINT{{X: sx - sc(4), Y: cy - sc(2)}, {X: sx + sc(4), Y: cy - sc(2)}, {X: sx, Y: cy + sc(3)}})
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

func headerOf(tv *walk.TableView) win.HWND {
	return win.HWND(win.SendMessage(tv.Handle(), win.LVM_GETHEADER, 0, 0))
}

// hookHeader subclasses the table so its header is custom drawn and reports arrow / right clicks.
func hookHeader(tv *walk.TableView, hk headerHooks) {
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
