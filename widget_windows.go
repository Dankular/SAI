//go:build windows

package main

import (
	"errors"
	"syscall"
	"unsafe"
)

// The overlay is a child of the Explorer window that hosts SHELLDLL_DefView, positioned above the
// icon view. Ordinary application windows still cover it. A layered color-key makes the renderer's
// empty pixels visually and interactively transparent while leaving the orb pixels draggable.

var (
	user32                       = syscall.NewLazyDLL("user32.dll")
	procFindWindowW              = user32.NewProc("FindWindowW")
	procFindWindowExW            = user32.NewProc("FindWindowExW")
	procSendMessageTimeoutW      = user32.NewProc("SendMessageTimeoutW")
	procEnumWindows              = user32.NewProc("EnumWindows")
	procEnumChildWindows         = user32.NewProc("EnumChildWindows")
	procGetWindowTextW           = user32.NewProc("GetWindowTextW")
	procGetWindowThreadProcessID = user32.NewProc("GetWindowThreadProcessId")
	procIsWindow                 = user32.NewProc("IsWindow")
	procIsWindowVisible          = user32.NewProc("IsWindowVisible")
	procGetParent                = user32.NewProc("GetParent")
	procSetParent                = user32.NewProc("SetParent")
	procGetWindowLongPtrW        = user32.NewProc("GetWindowLongPtrW")
	procSetWindowLongPtrW        = user32.NewProc("SetWindowLongPtrW")
	procSetWindowPos             = user32.NewProc("SetWindowPos")
	procGetWindowRect            = user32.NewProc("GetWindowRect")
	procShowWindow               = user32.NewProc("ShowWindow")
	procSetLayeredWindowAttrs    = user32.NewProc("SetLayeredWindowAttributes")
	procRedrawWindow             = user32.NewProc("RedrawWindow")
	procGetSystemMetrics         = user32.NewProc("GetSystemMetrics")
	procGetDpiForSystem          = user32.NewProc("GetDpiForSystem")
	procSetDpiAwarenessContext   = user32.NewProc("SetProcessDpiAwarenessContext")
	procPostMessageW             = user32.NewProc("PostMessageW")
	procSystemParametersInfoW    = user32.NewProc("SystemParametersInfoW")
)

const (
	gwlStyle   = -16
	gwlExStyle = -20

	wsPopup        = 0x80000000
	wsCaption      = 0x00C00000
	wsThickFrame   = 0x00040000
	wsSysMenu      = 0x00080000
	wsMinimizeBox  = 0x00020000
	wsMaximizeBox  = 0x00010000
	wsBorder       = 0x00800000
	wsChild        = 0x40000000
	wsVisible      = 0x10000000
	wsExToolWindow = 0x00000080
	wsExNoActivate = 0x08000000
	wsExAppWindow  = 0x00040000
	wsExLayered    = 0x00080000
	wsExWindowEdge = 0x00000100
	wsExClientEdge = 0x00000200

	swpNoActivate   = 0x0010
	swpShowWindow   = 0x0040
	swpFrameChanged = 0x0020

	wmClose = 0x0010
	swHide  = 0

	lwaColorKey = 0x00000001

	rdwInvalidate  = 0x0001
	rdwErase       = 0x0004
	rdwAllChildren = 0x0080
	rdwUpdateNow   = 0x0100

	widgetKeyColor = 0x00030201 // COLORREF for CSS #010203

	smCXScreen = 0
	smCYScreen = 1
)

type winRect struct{ Left, Top, Right, Bottom int32 }

func utf16Ptr(s string) *uint16 {
	if s == "" {
		return nil
	}
	p, _ := syscall.UTF16PtrFromString(s)
	return p
}

func findWindow(class string) uintptr {
	h, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(utf16Ptr(class))), 0)
	return h
}

func findWindowEx(parent, after uintptr, class string) uintptr {
	h, _, _ := procFindWindowExW.Call(parent, after, uintptr(unsafe.Pointer(utf16Ptr(class))), 0)
	return h
}

// enumTop calls visit for every top-level window until it returns false.
var enumVisit func(hwnd uintptr) bool
var enumCallback = syscall.NewCallback(func(hwnd, _ uintptr) uintptr {
	if enumVisit(hwnd) {
		return 1
	}
	return 0
})

var childEnumVisit func(hwnd uintptr) bool
var enumChildCallback = syscall.NewCallback(func(hwnd, _ uintptr) uintptr {
	if childEnumVisit(hwnd) {
		return 1
	}
	return 0
})

func enumTop(visit func(hwnd uintptr) bool) {
	enumVisit = visit
	procEnumWindows.Call(enumCallback, 0)
	enumVisit = nil
}

func enumAllWindows(visit func(hwnd uintptr) bool) {
	stopped := false
	enumTop(func(top uintptr) bool {
		if !visit(top) {
			stopped = true
			return false
		}
		childEnumVisit = func(child uintptr) bool {
			if !visit(child) {
				stopped = true
				return false
			}
			return true
		}
		procEnumChildWindows.Call(top, enumChildCallback, 0)
		childEnumVisit = nil
		return !stopped
	})
}

func windowText(hwnd uintptr) string {
	buf := make([]uint16, 256)
	n, _, _ := procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return syscall.UTF16ToString(buf[:n])
}

func windowPID(hwnd uintptr) uint32 {
	var pid uint32
	procGetWindowThreadProcessID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	return pid
}

func isWindow(hwnd uintptr) bool {
	r, _, _ := procIsWindow.Call(hwnd)
	return r != 0
}

func windowRect(hwnd uintptr) winRect {
	var r winRect
	procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	return r
}

// findWindowByTitle returns the top-level window whose title is exactly title.
func findWindowByTitle(title string) uintptr {
	var found uintptr
	enumTop(func(hwnd uintptr) bool {
		if windowText(hwnd) == title {
			found = hwnd
			return false
		}
		return true
	})
	return found
}

// closeWindowsTitled asks every window with this title to close and returns their process IDs.
func closeWindowsTitled(title string) []uint32 {
	var pids []uint32
	enumAllWindows(func(hwnd uintptr) bool {
		if windowText(hwnd) == title {
			pids = append(pids, windowPID(hwnd))
			procPostMessageW.Call(hwnd, wmClose, 0, 0)
		}
		return true
	})
	return pids
}

func windowWithPID(pid uint32) bool {
	found := false
	enumAllWindows(func(hwnd uintptr) bool {
		if windowPID(hwnd) == pid {
			found = true
			return false
		}
		return true
	})
	return found
}

// desktopIconHost finds the Explorer window that owns SHELLDLL_DefView. Hosting the orb above its
// child icon view makes the orb directly interactive without placing it over ordinary app windows.
func desktopIconHost() (uintptr, error) {
	progman := findWindow("Progman")
	if progman == 0 {
		return 0, errors.New("the desktop (Progman) window was not found")
	}
	var iconHost uintptr
	enumTop(func(hwnd uintptr) bool {
		if findWindowEx(hwnd, 0, "SHELLDLL_DefView") != 0 {
			iconHost = hwnd
			return false
		}
		return true
	})
	if iconHost != 0 {
		return iconHost, nil
	}
	// Some Windows 11 Explorer builds put SHELLDLL_DefView directly under Progman.
	if findWindowEx(progman, 0, "SHELLDLL_DefView") != 0 {
		return progman, nil
	}
	return 0, errors.New("could not find the Explorer window that hosts desktop icons")
}

// makeDPIAware lets this process see real pixel sizes, so positions line up on scaled displays.
func makeDPIAware() {
	procSetDpiAwarenessContext.Call(^uintptr(3)) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 (-4)
}

func systemDPI() int {
	if dpi, _, _ := procGetDpiForSystem.Call(); dpi != 0 {
		return int(dpi)
	}
	return 96
}

func primaryScreen() (w, h int) {
	cx, _, _ := procGetSystemMetrics.Call(smCXScreen)
	cy, _, _ := procGetSystemMetrics.Call(smCYScreen)
	return int(cx), int(cy)
}

func getLong(hwnd uintptr, index int) uintptr {
	v, _, _ := procGetWindowLongPtrW.Call(hwnd, uintptr(index))
	return v
}

func setLong(hwnd uintptr, index int, value uintptr) {
	procSetWindowLongPtrW.Call(hwnd, uintptr(index), value)
}

// embedOverlay turns Chromium's app window into a transparent child overlay above the desktop icons.
func embedOverlay(hwnd, parent uintptr, x, y, w, h int) error {
	style := getLong(hwnd, gwlStyle)
	style &^= wsPopup | wsCaption | wsThickFrame | wsSysMenu | wsMinimizeBox | wsMaximizeBox | wsBorder
	style |= wsChild | wsVisible
	setLong(hwnd, gwlStyle, style)
	if getLong(hwnd, gwlStyle)&(wsCaption|wsThickFrame|wsSysMenu|wsMinimizeBox|wsMaximizeBox|wsBorder) != 0 {
		return errors.New("could not remove the browser window frame")
	}
	ex := getLong(hwnd, gwlExStyle)
	ex &^= wsExAppWindow | wsExWindowEdge | wsExClientEdge
	ex |= wsExToolWindow | wsExNoActivate | wsExLayered
	setLong(hwnd, gwlExStyle, ex)
	procSetParent.Call(hwnd, parent)
	if parentOf(hwnd) != parent {
		return errors.New("could not attach the orb to the desktop icon host")
	}
	if ok, _, callErr := procSetLayeredWindowAttrs.Call(hwnd, widgetKeyColor, 255, lwaColorKey); ok == 0 {
		if callErr != nil {
			return callErr
		}
		return errors.New("could not enable transparent orb hit testing")
	}
	if !placeOverlay(hwnd, parent, x, y, w, h) {
		return errors.New("could not position the desktop orb overlay")
	}
	return nil
}

// placeOverlay moves the child overlay in desktop screen coordinates and keeps it above the icons.
func placeOverlay(hwnd, parent uintptr, x, y, w, h int) bool {
	origin := windowRect(parent)
	const hwndTop = 0 // HWND_TOP: above the desktop icon view among the host's child windows.
	ok, _, _ := procSetWindowPos.Call(hwnd, hwndTop, uintptr(x-int(origin.Left)), uintptr(y-int(origin.Top)), uintptr(w), uintptr(h), swpNoActivate|swpShowWindow|swpFrameChanged)
	return ok != 0
}

func redrawDesktop(parent uintptr) {
	procRedrawWindow.Call(parent, 0, 0, rdwInvalidate|rdwErase|rdwAllChildren|rdwUpdateNow)
	if progman := findWindow("Progman"); progman != 0 && progman != parent {
		procRedrawWindow.Call(progman, 0, 0, rdwInvalidate|rdwErase|rdwAllChildren|rdwUpdateNow)
	}
}

func parentOf(hwnd uintptr) uintptr {
	p, _, _ := procGetParent.Call(hwnd)
	return p
}

// workArea is the primary screen minus the taskbar, in pixels.
func workArea() (x, y, w, h int) {
	var r winRect
	if ok, _, _ := procSystemParametersInfoW.Call(0x0030, 0, uintptr(unsafe.Pointer(&r)), 0); ok == 0 {
		sw, sh := primaryScreen()
		return 0, 0, sw, sh
	}
	return int(r.Left), int(r.Top), int(r.Right - r.Left), int(r.Bottom - r.Top)
}
