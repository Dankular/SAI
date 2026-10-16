//go:build windows

package main

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	wsExLayeredNative    = 0x00080000
	wsExToolWindowNative = 0x00000080
	wsExNoActivateNative = 0x08000000
	wsExAppWindowNative  = 0x00040000
	wsExWindowEdgeNative = 0x00000100
	wsExClientEdgeNative = 0x00000200
	wsChildNative        = 0x40000000
	wsVisibleNative      = 0x10000000
	wsPopupNative        = 0x80000000

	swpNoSizeNative        = 0x0001
	swpNoZOrderNative      = 0x0004
	swpNoOwnerZOrderNative = 0x0200

	wmCloseNative          = 0x0010
	wmDestroyNative        = 0x0002
	wmMouseMoveNative      = 0x0200
	wmLeftButtonDownNative = 0x0201
	wmLeftButtonUpNative   = 0x0202
	wmCaptureChangedNative = 0x0215
	wmDisplayChangeNative  = 0x007e

	pmRemoveNative         = 0x0001
	ulwAlphaNative         = 0x00000002
	biRGBNative            = 0
	dibRGBColorsNative     = 0
	blendSourceAlphaNative = 1
)

const nativeOrbWindowClass = "SovereignNativeOrb"

var (
	procRegisterClassExNative     = user32.NewProc("RegisterClassExW")
	procCreateWindowExNative      = user32.NewProc("CreateWindowExW")
	procDefWindowProcNative       = user32.NewProc("DefWindowProcW")
	procDestroyWindowNative       = user32.NewProc("DestroyWindow")
	procPeekMessageNative         = user32.NewProc("PeekMessageW")
	procTranslateMessageNative    = user32.NewProc("TranslateMessage")
	procDispatchMessageNative     = user32.NewProc("DispatchMessageW")
	procGetCursorPosNative        = user32.NewProc("GetCursorPos")
	procSetCaptureNative          = user32.NewProc("SetCapture")
	procReleaseCaptureNative      = user32.NewProc("ReleaseCapture")
	procUpdateLayeredWindowNative = user32.NewProc("UpdateLayeredWindow")
	procPostMessageNative         = user32.NewProc("PostMessageW")
	procGetModuleHandleNative     = syscall.NewLazyDLL("kernel32.dll").NewProc("GetModuleHandleW")
	procGetLastErrorNative        = syscall.NewLazyDLL("kernel32.dll").NewProc("GetLastError")
	procMonitorFromWindowNative   = user32.NewProc("MonitorFromWindow")
	procGetMonitorInfoNative      = user32.NewProc("GetMonitorInfoW")
	procEnumDisplaySettingsNative = user32.NewProc("EnumDisplaySettingsExW")
	procCreateCompatibleDCNative  = syscall.NewLazyDLL("gdi32.dll").NewProc("CreateCompatibleDC")
	procDeleteDCNative            = syscall.NewLazyDLL("gdi32.dll").NewProc("DeleteDC")
	procCreateDIBSectionNative    = syscall.NewLazyDLL("gdi32.dll").NewProc("CreateDIBSection")
	procSelectObjectNative        = syscall.NewLazyDLL("gdi32.dll").NewProc("SelectObject")
	procDeleteObjectNative        = syscall.NewLazyDLL("gdi32.dll").NewProc("DeleteObject")

	nativeWidgetProc = syscall.NewCallback(nativeWidgetWindowProc)
)

type nativeWidgetRenderer struct {
	mu      sync.Mutex
	windows map[uintptr]*nativeOrbWindow
}

type nativeOrbWindow struct {
	app          *app
	renderer     *nativeWidgetRenderer
	hwnd         uintptr
	width        int
	height       int
	memDC        uintptr
	bitmap       uintptr
	oldBitmap    uintptr
	pixels       []byte
	startedAt    time.Time
	lastRenderAt time.Time
	refreshHz    int
	audio        orbAudioEnvelope
	drag         nativeOrbDrag
	stop         chan struct{}
	stopOnce     sync.Once
	done         chan struct{}
}

type nativeOrbDrag struct {
	active       bool
	moved        bool
	windowStartX int
	windowStartY int
	cursorStartX int
	cursorStartY int
}

type nativePoint struct{ X, Y int32 }
type nativeSize struct{ CX, CY int32 }

type nativeRenderedOrbFrame struct {
	pixels []byte
	at     time.Time
}

type nativeMonitorInfoEx struct {
	Size       uint32
	Monitor    winRect
	Work       winRect
	Flags      uint32
	DeviceName [32]uint16
}

// nativeDevMode mirrors the fixed DEVMODEW layout through its refresh-rate fields.
type nativeDevMode struct {
	DeviceName       [32]uint16
	SpecVersion      uint16
	DriverVersion    uint16
	Size             uint16
	DriverExtra      uint16
	Fields           uint32
	DisplayUnion     [4]uint32
	Color            int16
	Duplex           int16
	YResolution      int16
	TTOption         int16
	Collate          int16
	FormName         [32]uint16
	LogPixels        uint16
	BitsPerPel       uint32
	PelsWidth        uint32
	PelsHeight       uint32
	DisplayFlags     uint32
	DisplayFrequency uint32
	ICMMethod        uint32
	ICMIntent        uint32
	MediaType        uint32
	DitherType       uint32
	Reserved1        uint32
	Reserved2        uint32
	PanningWidth     uint32
	PanningHeight    uint32
}

type nativeBitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

type nativeBitmapInfo struct {
	Header nativeBitmapInfoHeader
	Color  uint32
}

type nativeBlendFunction struct {
	Operation   byte
	Flags       byte
	SourceAlpha byte
	AlphaFormat byte
}

type nativeWindowClass struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClassExtra int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSmall  uintptr
}

type nativeMessage struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Point   nativePoint
	Private uint32
}

func (r *nativeWidgetRenderer) start(a *app, x, y, width, height int) (*exec.Cmd, uintptr, error) {
	r.mu.Lock()
	if r.windows == nil {
		r.windows = make(map[uintptr]*nativeOrbWindow)
	}
	r.mu.Unlock()
	w := &nativeOrbWindow{
		app: a, renderer: r, width: width, height: height,
		startedAt: time.Now(), stop: make(chan struct{}), done: make(chan struct{}),
	}
	ready := make(chan error, 1)
	go w.run(x, y, ready)
	if err := <-ready; err != nil {
		return nil, 0, err
	}
	r.mu.Lock()
	r.windows[w.hwnd] = w
	r.mu.Unlock()
	return nil, w.hwnd, nil
}

func (r *nativeWidgetRenderer) embed(hwnd, _ uintptr, x, y, width, height int) error {
	r.mu.Lock()
	w := r.windows[hwnd]
	r.mu.Unlock()
	if w == nil || !isWindow(hwnd) {
		return errors.New("the native orb window disappeared before it was shown")
	}
	style := getLong(hwnd, gwlStyle)
	style &^= wsChildNative | wsCaption | wsThickFrame | wsSysMenu | wsMinimizeBox | wsMaximizeBox | wsBorder
	style |= wsPopupNative | wsVisibleNative
	setLong(hwnd, gwlStyle, style)
	ex := getLong(hwnd, gwlExStyle)
	ex &^= wsExAppWindowNative | wsExWindowEdgeNative | wsExClientEdgeNative
	ex |= wsExToolWindowNative | wsExNoActivateNative | wsExLayeredNative
	setLong(hwnd, gwlExStyle, ex)
	if !placeBottomNativeOverlay(hwnd, x, y, width, height) {
		return errors.New("could not position the native desktop orb")
	}
	procShowWindow.Call(hwnd, 4) // SW_SHOWNA
	return nil
}

func placeBottomNativeOverlay(hwnd uintptr, x, y, width, height int) bool {
	const hwndBottomNative = 1
	ok, _, _ := procSetWindowPos.Call(hwnd, hwndBottomNative, uintptr(x), uintptr(y), uintptr(width), uintptr(height), swpNoActivate|swpShowWindow|swpFrameChanged)
	return ok != 0
}

func nativeDragPositionFlags() uintptr {
	return swpNoActivate | swpNoSizeNative | swpNoZOrderNative | swpNoOwnerZOrderNative
}

func moveNativeOverlay(hwnd uintptr, x, y int) bool {
	ok, _, _ := procSetWindowPos.Call(hwnd, 0, uintptr(x), uintptr(y), 0, 0, nativeDragPositionFlags())
	return ok != 0
}

func (r *nativeWidgetRenderer) stop(hwnd uintptr, _ *exec.Cmd) {
	r.mu.Lock()
	w := r.windows[hwnd]
	delete(r.windows, hwnd)
	r.mu.Unlock()
	if w == nil {
		return
	}
	w.stopOnce.Do(func() { close(w.stop) })
	select {
	case <-w.done:
	case <-time.After(3 * time.Second):
		procPostMessageNative.Call(hwnd, wmCloseNative, 0, 0)
		<-w.done
	}
}

func (w *nativeOrbWindow) run(x, y int, ready chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(w.done)
	if err := registerNativeOrbClass(); err != nil {
		ready <- err
		return
	}
	ex := uintptr(wsExLayeredNative | wsExToolWindowNative | wsExNoActivateNative)
	hwnd, _, callErr := procCreateWindowExNative.Call(
		ex,
		uintptr(unsafe.Pointer(utf16Ptr(nativeOrbWindowClass))),
		uintptr(unsafe.Pointer(utf16Ptr(""))),
		wsPopupNative,
		uintptr(x), uintptr(y), uintptr(w.width), uintptr(w.height),
		0, 0, w.instance(), 0,
	)
	if hwnd == 0 {
		if callErr == nil || callErr == syscall.Errno(0) {
			lastError, _, _ := procGetLastErrorNative.Call()
			callErr = syscall.Errno(lastError)
		}
		ready <- fmt.Errorf("create the native orb window: %w", callErr)
		return
	}
	w.hwnd = hwnd
	defer w.destroy()
	nativeWidgetRenderers.Store(hwnd, w)
	w.refreshHz = nativeDisplayRefreshRate(hwnd)
	if err := w.createSurface(); err != nil {
		ready <- err
		return
	}
	initialFrame := w.renderFrame(time.Now())
	if err := w.displayFrame(initialFrame.pixels); err != nil {
		ready <- err
		return
	}
	procShowWindow.Call(hwnd, swHide)
	ready <- nil
	frameUpdates := make(chan nativeRenderedOrbFrame, 2)
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		w.produceFrames(frameUpdates)
	}()
	defer func() {
		w.stopOnce.Do(func() { close(w.stop) })
		<-producerDone
	}()

	frameTick := time.NewTimer(nativeFrameInterval(w.refreshHz))
	defer frameTick.Stop()
	refreshCheck := time.NewTicker(time.Second)
	defer refreshCheck.Stop()
	frameInterval := nativeFrameInterval(w.refreshHz)
	nextPresentation := time.Now().Add(frameInterval)
	previousFrame, currentFrame := initialFrame, initialFrame
	for {
		w.dispatchMessages()
		if next := nativeFrameInterval(w.refreshHz); next != frameInterval {
			if !frameTick.Stop() {
				select {
				case <-frameTick.C:
				default:
				}
			}
			frameInterval = next
			nextPresentation = time.Now().Add(frameInterval)
			frameTick.Reset(time.Until(nextPresentation))
		}
		select {
		case <-w.stop:
			return
		case <-frameTick.C:
			if isWindow(w.hwnd) {
				for {
					select {
					case frame := <-frameUpdates:
						previousFrame, currentFrame = currentFrame, frame
					default:
						goto framesCollected
					}
				}
			framesCollected:
				now := time.Now()
				if err := w.displayInterpolated(previousFrame, currentFrame, now); err != nil {
					w.app.recordCommandOutput("Desktop orb render failed: " + err.Error())
					w.stopOnce.Do(func() { close(w.stop) })
					return
				}
			} else {
				return
			}
			nextPresentation = nextPresentation.Add(frameInterval)
			if now := time.Now(); !nextPresentation.After(now) {
				skipped := int64(now.Sub(nextPresentation)/frameInterval) + 1
				nextPresentation = nextPresentation.Add(time.Duration(skipped) * frameInterval)
			}
			frameTick.Reset(time.Until(nextPresentation))
		case <-refreshCheck.C:
			w.refreshHz = nativeDisplayRefreshRate(w.hwnd)
		}
	}
}

// produceFrames renders a steady stream of source frames. The native window blends those frames at
// the display's reported refresh rate, so expensive scene rendering does not have to finish every
// 6–8 ms on high-refresh displays.
func (w *nativeOrbWindow) produceFrames(out chan<- nativeRenderedOrbFrame) {
	const renderInterval = time.Second / 60
	nextRender := w.lastRenderAt.Add(renderInterval)
	for {
		if wait := time.Until(nextRender); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-w.stop:
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		frame := w.renderFrame(time.Now())
		select {
		case out <- frame:
		case <-w.stop:
			return
		}
		nextRender = nextRender.Add(renderInterval)
		if now := time.Now(); !nextRender.After(now) {
			nextRender = now
		}
	}
}

func nativeFrameInterval(refreshHz int) time.Duration {
	if refreshHz < 2 || refreshHz > 1000 {
		refreshHz = 60
	}
	interval := time.Second / time.Duration(refreshHz)
	if interval < time.Millisecond {
		return time.Millisecond
	}
	return interval
}

func nativeDisplayRefreshRate(hwnd uintptr) int {
	var device uintptr
	if monitor, _, _ := procMonitorFromWindowNative.Call(hwnd, 2); monitor != 0 {
		info := nativeMonitorInfoEx{Size: uint32(unsafe.Sizeof(nativeMonitorInfoEx{}))}
		if ok, _, _ := procGetMonitorInfoNative.Call(monitor, uintptr(unsafe.Pointer(&info))); ok != 0 {
			device = uintptr(unsafe.Pointer(&info.DeviceName[0]))
		}
	}
	mode := nativeDevMode{Size: uint16(unsafe.Sizeof(nativeDevMode{}))}
	if ok, _, _ := procEnumDisplaySettingsNative.Call(device, uintptr(0xffffffff), uintptr(unsafe.Pointer(&mode)), 0); ok != 0 {
		if mode.DisplayFrequency >= 2 && mode.DisplayFrequency <= 1000 {
			return int(mode.DisplayFrequency)
		}
	}
	return 60
}

func (w *nativeOrbWindow) instance() uintptr {
	instance, _, _ := procGetModuleHandleNative.Call(0)
	return instance
}

func registerNativeOrbClass() error {
	instance, _, _ := procGetModuleHandleNative.Call(0)
	className := utf16Ptr(nativeOrbWindowClass)
	wc := nativeWindowClass{
		Size: uint32(unsafe.Sizeof(nativeWindowClass{})), WndProc: nativeWidgetProc,
		Instance: instance, ClassName: className,
	}
	atom, _, callErr := procRegisterClassExNative.Call(uintptr(unsafe.Pointer(&wc)))
	if atom != 0 {
		return nil
	}
	var lastError uintptr
	if errno, ok := callErr.(syscall.Errno); ok {
		lastError = uintptr(errno)
	}
	if lastError == 0 {
		lastError, _, _ = procGetLastErrorNative.Call()
	}
	if lastError == 1410 { // ERROR_CLASS_ALREADY_EXISTS
		return nil
	}
	return fmt.Errorf("register the native orb window class (size %d, error %d): %w", wc.Size, lastError, syscall.Errno(lastError))
}

func (w *nativeOrbWindow) createSurface() error {
	memDC, _, _ := procCreateCompatibleDCNative.Call(0)
	if memDC == 0 {
		return errors.New("could not create a drawing surface for the native orb")
	}
	w.memDC = memDC
	info := nativeBitmapInfo{Header: nativeBitmapInfoHeader{
		Size: uint32(unsafe.Sizeof(nativeBitmapInfoHeader{})), Width: int32(w.width), Height: -int32(w.height),
		Planes: 1, BitCount: 32, Compression: biRGBNative,
		SizeImage: uint32(w.width * w.height * 4),
	}}
	var bits unsafe.Pointer
	hbitmap, _, _ := procCreateDIBSectionNative.Call(memDC, uintptr(unsafe.Pointer(&info)), dibRGBColorsNative, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if hbitmap == 0 || bits == nil {
		return errors.New("could not create the native orb's transparent bitmap")
	}
	w.bitmap = hbitmap
	w.oldBitmap, _, _ = procSelectObjectNative.Call(memDC, hbitmap)
	w.pixels = unsafe.Slice((*byte)(bits), w.width*w.height*4)
	return nil
}

func (w *nativeOrbWindow) renderFrame(now time.Time) nativeRenderedOrbFrame {
	inputs := w.app.orbRenderInputs()
	delta := 0.0
	if !w.lastRenderAt.IsZero() {
		delta = now.Sub(w.lastRenderAt).Seconds()
	}
	w.lastRenderAt = now
	w.audio.update(&inputs, delta)
	return nativeRenderedOrbFrame{
		pixels: renderOrbFrameWithInputs(w.width, w.height, inputs, now.Sub(w.startedAt).Seconds()),
		at:     now,
	}
}

func (w *nativeOrbWindow) displayFrame(frame []byte) error {
	copy(w.pixels, frame)
	return w.updateLayeredSurface()
}

func (w *nativeOrbWindow) displayInterpolated(from, to nativeRenderedOrbFrame, now time.Time) error {
	amount := 1.0
	gap := to.at.Sub(from.at)
	if gap > 0 {
		// Present one source-frame behind, then blend forward across the interval to the next sample.
		amount = float64(now.Add(-gap).Sub(from.at)) / float64(gap)
		amount = max(0, min(1, amount))
	}
	blendNativeOrbFrames(w.pixels, from.pixels, to.pixels, amount)
	return w.updateLayeredSurface()
}

func blendNativeOrbFrames(destination, from, to []byte, amount float64) {
	if len(destination) != len(from) || len(from) != len(to) {
		return
	}
	if amount <= 0 {
		copy(destination, from)
		return
	}
	if amount >= 1 {
		copy(destination, to)
		return
	}
	weight := uint32(amount * 65536)
	inverse := uint32(65536) - weight
	for i := range destination {
		value := uint32(from[i])*inverse + uint32(to[i])*weight
		destination[i] = byte((value + 32768) >> 16)
	}
}

func (w *nativeOrbWindow) updateLayeredSurface() error {
	rect := windowRect(w.hwnd)
	destination := nativePoint{X: rect.Left, Y: rect.Top}
	size := nativeSize{CX: int32(w.width), CY: int32(w.height)}
	source := nativePoint{}
	blend := nativeBlendFunction{SourceAlpha: 255, AlphaFormat: blendSourceAlphaNative}
	ok, _, callErr := procUpdateLayeredWindowNative.Call(
		w.hwnd, 0, uintptr(unsafe.Pointer(&destination)), uintptr(unsafe.Pointer(&size)), w.memDC,
		uintptr(unsafe.Pointer(&source)), 0, uintptr(unsafe.Pointer(&blend)), ulwAlphaNative,
	)
	if ok == 0 {
		if callErr != nil && callErr != syscall.Errno(0) {
			return fmt.Errorf("update the native orb surface: %w", callErr)
		}
		return errors.New("update the native orb surface failed")
	}
	return nil
}

func (w *nativeOrbWindow) dispatchMessages() {
	var msg nativeMessage
	deadline := time.Now().Add(2 * time.Millisecond)
	for time.Now().Before(deadline) {
		if result, _, _ := procPeekMessageNative.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0, pmRemoveNative); result == 0 {
			return
		}
		procTranslateMessageNative.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessageNative.Call(uintptr(unsafe.Pointer(&msg)))
	}
}

func (w *nativeOrbWindow) destroy() {
	if isWindow(w.hwnd) {
		procDestroyWindowNative.Call(w.hwnd)
	}
	nativeWidgetRenderers.Delete(w.hwnd)
	if w.oldBitmap != 0 && w.memDC != 0 {
		procSelectObjectNative.Call(w.memDC, w.oldBitmap)
		w.oldBitmap = 0
	}
	if w.bitmap != 0 {
		procDeleteObjectNative.Call(w.bitmap)
		w.bitmap = 0
	}
	if w.memDC != 0 {
		procDeleteDCNative.Call(w.memDC)
		w.memDC = 0
	}
	w.pixels = nil
}

func (a *app) orbRenderInputs() orbFrameInputs {
	snapshot := a.orbShown.snapshot()
	inputs := orbFrameInputs{
		State: snapshot.State, PreviousState: snapshot.PreviousState, ChangedAgeMs: snapshot.ChangedAgeMs,
		Waking: snapshot.Waking, StimulusAgeMs: snapshot.StimulusAgeMs, FlashAgeMs: snapshot.FlashAgeMs,
	}
	if inputs.State == "" {
		inputs.State, inputs.ChangedAgeMs = "idle", -1
	}
	if snapshot.LevelAgeMs >= 0 && snapshot.LevelAgeMs < 300 {
		inputs.Level, inputs.Bass, inputs.Mid, inputs.Treble = snapshot.Level, snapshot.Bass, snapshot.Mid, snapshot.Treble
	}
	inputs.StimulusAgeMs, inputs.FlashAgeMs = snapshot.StimulusAgeMs, snapshot.FlashAgeMs
	return inputs
}

func nativeWidgetWindowProc(hwnd uintptr, message uint32, wParam, lParam uintptr) uintptr {
	if message == wmDestroyNative {
		return 0
	}
	window, ok := nativeWidgetRenderers.Load(hwnd)
	if !ok {
		result, _, _ := procDefWindowProcNative.Call(hwnd, uintptr(message), wParam, lParam)
		return result
	}
	orbWindow := window.(*nativeOrbWindow)
	switch message {
	case wmDisplayChangeNative:
		orbWindow.refreshHz = nativeDisplayRefreshRate(hwnd)
		return 0
	case wmCloseNative:
		orbWindow.stopOnce.Do(func() { close(orbWindow.stop) })
		return 0
	case wmLeftButtonDownNative:
		orbWindow.beginDrag(hwnd)
		return 0
	case wmMouseMoveNative:
		orbWindow.moveDrag(hwnd)
		return 0
	case wmLeftButtonUpNative:
		orbWindow.endDrag(hwnd)
		return 0
	case wmCaptureChangedNative:
		orbWindow.endDrag(hwnd)
		return 0
	}
	result, _, _ := procDefWindowProcNative.Call(hwnd, uintptr(message), wParam, lParam)
	return result
}

var nativeWidgetRenderers sync.Map // HWND -> *nativeOrbWindow, available during CreateWindow messages too.

func (w *nativeOrbWindow) beginDrag(hwnd uintptr) {
	var cursor nativePoint
	procGetCursorPosNative.Call(uintptr(unsafe.Pointer(&cursor)))
	rect := windowRect(hwnd)
	w.drag = nativeOrbDrag{
		active: true, windowStartX: int(rect.Left), windowStartY: int(rect.Top),
		cursorStartX: int(cursor.X), cursorStartY: int(cursor.Y),
	}
	procSetCaptureNative.Call(hwnd)
}

func (w *nativeOrbWindow) moveDrag(hwnd uintptr) {
	if !w.drag.active {
		return
	}
	var cursor nativePoint
	procGetCursorPosNative.Call(uintptr(unsafe.Pointer(&cursor)))
	dx, dy := int(cursor.X)-w.drag.cursorStartX, int(cursor.Y)-w.drag.cursorStartY
	if dx == 0 && dy == 0 {
		return
	}
	w.drag.moved = true
	ax, ay, aw, ah := workArea()
	maxX, maxY := max(0, aw-w.width), max(0, ah-w.height)
	x := max(ax, min(ax+maxX, w.drag.windowStartX+dx))
	y := max(ay, min(ay+maxY, w.drag.windowStartY+dy))
	positionX, positionY := 0.0, 0.0
	if maxX > 0 {
		positionX = float64(x-ax) / float64(maxX)
	}
	if maxY > 0 {
		positionY = float64(y-ay) / float64(maxY)
	}
	moveNativeOverlay(hwnd, x, y)
	w.app.updateWidgetPosition(positionX, positionY, true)
}

func (w *nativeOrbWindow) endDrag(hwnd uintptr) {
	if !w.drag.active {
		return
	}
	wasMoved := w.drag.moved
	w.drag.active = false
	procReleaseCaptureNative.Call()
	if !wasMoved {
		return
	}
	w.app.mu.Lock()
	positionX, positionY := w.app.state.WidgetPositionX, w.app.state.WidgetPositionY
	w.app.mu.Unlock()
	w.app.updateWidgetPosition(positionX, positionY, false)
}
