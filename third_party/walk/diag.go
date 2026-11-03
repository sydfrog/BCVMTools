// +build windows

package walk

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

// Local addition (BCVMTools): a small ring buffer of the most recent window messages seen by the
// main loop, so a hang inside the message pump can be traced to the message that caused it.

type recordedMsg struct {
	hwnd   uintptr
	msg    uint32
	wp, lp uintptr
	at     int64
}

var (
	diagRing [32]recordedMsg
	diagPos  uint32
)

func recordMsg(hwnd uintptr, msg uint32, wp, lp uintptr) {
	i := atomic.AddUint32(&diagPos, 1) % uint32(len(diagRing))
	diagRing[i] = recordedMsg{hwnd, msg, wp, lp, time.Now().UnixNano()}
}

var msgNames = map[uint32]string{
	0x000F: "WM_PAINT", 0x0100: "WM_KEYDOWN", 0x0101: "WM_KEYUP", 0x0102: "WM_CHAR", 0x0104: "WM_SYSKEYDOWN",
	0x0105: "WM_SYSKEYUP", 0x0106: "WM_SYSCHAR", 0x0111: "WM_COMMAND", 0x0113: "WM_TIMER", 0x0200: "WM_MOUSEMOVE",
	0x0201: "WM_LBUTTONDOWN", 0x0202: "WM_LBUTTONUP", 0x0204: "WM_RBUTTONDOWN", 0x0205: "WM_RBUTTONUP",
	0x00A0: "WM_NCMOUSEMOVE", 0x00A1: "WM_NCLBUTTONDOWN", 0x02A3: "WM_MOUSELEAVE", 0x0281: "WM_IME_SETCONTEXT",
	0x0007: "WM_SETFOCUS", 0x0008: "WM_KILLFOCUS", 0x0020: "WM_SETCURSOR", 0x0084: "WM_NCHITTEST", 0x001C: "WM_ACTIVATEAPP",
	0x0006: "WM_ACTIVATE", 0x0286: "WM_IME_CHAR", 0x0319: "WM_APPCOMMAND", 0x020A: "WM_MOUSEWHEEL",
}

// RecentMessages returns the last few messages the main loop pulled from the queue (oldest first).
func RecentMessages() string {
	pos := atomic.LoadUint32(&diagPos)
	var sb strings.Builder
	n := uint32(len(diagRing))
	for k := uint32(12); k > 0; k-- {
		r := diagRing[(pos+n-(k-1))%n]
		if r.at == 0 {
			continue
		}
		name := msgNames[r.msg]
		if name == "" {
			name = fmt.Sprintf("0x%04x", r.msg)
		}
		fmt.Fprintf(&sb, "    %s hwnd=0x%x wParam=0x%x lParam=0x%x\n", time.Unix(0, r.at).Format("15:04:05.000"), r.hwnd, r.wp, r.lp)
		sb.WriteString("      ")
		sb.WriteString(name)
		sb.WriteString("\n")
	}
	return sb.String()
}
