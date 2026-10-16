//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// The desktop orb is a per-pixel alpha native overlay kept at the bottom of the normal app z-order.
// Chromium stays available behind the renderer boundary; the native renderer avoids browser chrome
// and lets empty pixels pass through to the desktop.

const widgetTitle = "Sovereign Widget" // must match <title> in web/widget.html

// widgetRenderer separates the desktop host from the technology that draws the orb.
type widgetRenderer interface {
	start(a *app, x, y, w, h int) (*exec.Cmd, uintptr, error)
	embed(hwnd, parent uintptr, x, y, w, h int) error
	stop(hwnd uintptr, cmd *exec.Cmd)
}

type widgetRuntime struct {
	mu       sync.Mutex
	cmd      *exec.Cmd
	hwnd     uintptr
	parent   uintptr
	renderer widgetRenderer
}

var dpiOnce sync.Once

func widgetBrowser() (string, error) {
	var candidates []string
	for _, root := range []string{os.Getenv("ProgramFiles(x86)"), os.Getenv("ProgramFiles"), os.Getenv("LocalAppData")} {
		if root == "" {
			continue
		}
		candidates = append(candidates,
			filepath.Join(root, "Microsoft", "Edge", "Application", "msedge.exe"),
			filepath.Join(root, "Google", "Chrome", "Application", "chrome.exe"))
	}
	for _, c := range candidates {
		if isFile(c) {
			return c, nil
		}
	}
	return "", errors.New("Microsoft Edge was not found, and the desktop widget needs it")
}

// widgetLayout returns the pixel position and size for the saved corner and size.
func (a *app) widgetLayout() (x, y, w, h int) {
	a.mu.Lock()
	corner, size := a.state.WidgetCorner, a.state.WidgetSize
	positionSet, positionX, positionY := a.state.WidgetPositionSet, a.state.WidgetPositionX, a.state.WidgetPositionY
	a.mu.Unlock()
	dims, ok := widgetSizes[size]
	if !ok {
		dims = widgetSizes["medium"]
	}
	scale := float64(systemDPI()) / 96
	w, h = int(float64(dims[0])*scale), int(float64(dims[1])*scale)
	margin := int(24 * scale)
	ax, ay, aw, ah := workArea()
	if positionSet {
		maxX, maxY := max(0, aw-w), max(0, ah-h)
		return ax + int(clampUnit(positionX)*float64(maxX)), ay + int(clampUnit(positionY)*float64(maxY)), w, h
	}
	x, y = ax+aw-w-margin, ay+margin // top-right
	switch corner {
	case "top-left":
		x = ax + margin
	case "bottom-left":
		x, y = ax+margin, ay+ah-h-margin
	case "bottom-right":
		y = ay + ah - h - margin
	}
	return
}

func (a *app) startWidget() error {
	dpiOnce.Do(makeDPIAware)
	renderer := a.widget.renderer
	if renderer == nil {
		renderer = &nativeWidgetRenderer{}
	}
	// Close any orphaned widget window from an earlier forced app exit before finding ours.
	for _, pid := range closeWindowsTitled(widgetTitle) {
		deadline := time.Now().Add(2 * time.Second)
		for windowWithPID(pid) && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		if windowWithPID(pid) {
			_ = exec.Command("taskkill.exe", "/PID", fmt.Sprint(pid), "/T", "/F").Run()
		}
	}
	x, y, w, h := a.widgetLayout()
	var parent uintptr
	if _, native := renderer.(*nativeWidgetRenderer); !native {
		var err error
		parent, err = desktopIconHost()
		if err != nil {
			return err
		}
	}
	cmd, hwnd, err := renderer.start(a, x, y, w, h)
	if err != nil {
		return err
	}
	if err := renderer.embed(hwnd, parent, x, y, w, h); err != nil {
		renderer.stop(hwnd, cmd)
		redrawDesktop(parent)
		return err
	}
	a.widget.mu.Lock()
	a.widget.cmd, a.widget.hwnd, a.widget.parent, a.widget.renderer = cmd, hwnd, parent, renderer
	a.widget.mu.Unlock()
	a.recordCommandOutput("Desktop orb is showing")
	return nil
}

func (a *app) stopWidget() {
	a.widget.mu.Lock()
	hwnd, cmd, parent, renderer := a.widget.hwnd, a.widget.cmd, a.widget.parent, a.widget.renderer
	a.widget.cmd, a.widget.hwnd, a.widget.parent, a.widget.renderer = nil, 0, 0, nil
	a.widget.mu.Unlock()
	if renderer == nil {
		renderer = &nativeWidgetRenderer{}
	}
	renderer.stop(hwnd, cmd)
	if isWindow(parent) {
		redrawDesktop(parent)
	}
}

// widgetHealthy is true while the overlay is still attached to its expected desktop host.
func (a *app) widgetHealthy() bool {
	a.widget.mu.Lock()
	hwnd, parent := a.widget.hwnd, a.widget.parent
	a.widget.mu.Unlock()
	if hwnd == 0 || !isWindow(hwnd) || parentOf(hwnd) != parent {
		return false
	}
	return parent == 0 || isWindow(parent)
}

// applyWidgetLayout moves a running widget after a drag, corner, or size change.
func (a *app) applyWidgetLayout() {
	a.widget.mu.Lock()
	hwnd, parent, renderer := a.widget.hwnd, a.widget.parent, a.widget.renderer
	a.widget.mu.Unlock()
	if hwnd == 0 || !isWindow(hwnd) {
		return
	}
	x, y, w, h := a.widgetLayout()
	if _, native := renderer.(*nativeWidgetRenderer); native {
		placeBottomNativeOverlay(hwnd, x, y, w, h)
		return
	}
	if !isWindow(parent) {
		return
	}
	placeOverlay(hwnd, parent, x, y, w, h)
}

type chromiumWidgetRenderer struct{}

func (chromiumWidgetRenderer) start(a *app, x, y, w, h int) (*exec.Cmd, uintptr, error) {
	browser, err := widgetBrowser()
	if err != nil {
		return nil, 0, err
	}
	profile := filepath.Join(a.dataDir, "widget-profile")
	scale := float64(systemDPI()) / 96
	cmd := exec.Command(browser,
		"--app="+a.serverURL+"/widget.html",
		"--user-data-dir="+profile,
		fmt.Sprintf("--window-size=%d,%d", int(float64(w)/scale), int(float64(h)/scale)),
		fmt.Sprintf("--window-position=%d,%d", x, y),
		"--no-first-run", "--no-default-browser-check", "--disable-extensions", "--disable-sync",
		"--disable-background-networking", "--mute-audio",
		"--disable-features=CalculateNativeWinOcclusion,msEdgeSidebar,Translate",
		"--disable-backgrounding-occluded-windows", "--disable-renderer-backgrounding", "--disable-background-timer-throttling",
	)
	if err := cmd.Start(); err != nil {
		return nil, 0, fmt.Errorf("start the widget window: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		if a.ctx.Err() != nil {
			_ = exec.Command("taskkill.exe", "/PID", fmt.Sprint(cmd.Process.Pid), "/T", "/F").Run()
			return cmd, 0, a.ctx.Err()
		}
		if hwnd := findWindowByTitle(widgetTitle); hwnd != 0 {
			procShowWindow.Call(hwnd, swHide)
			return cmd, hwnd, nil
		}
	}
	_ = exec.Command("taskkill.exe", "/PID", fmt.Sprint(cmd.Process.Pid), "/T", "/F").Run()
	return cmd, 0, errors.New("the widget window did not appear")
}

func (chromiumWidgetRenderer) embed(hwnd, parent uintptr, x, y, w, h int) error {
	return embedOverlay(hwnd, parent, x, y, w, h)
}

func (chromiumWidgetRenderer) stop(hwnd uintptr, cmd *exec.Cmd) {
	if hwnd == 0 {
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return
	}
	procPostMessageW.Call(hwnd, wmClose, 0, 0)
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if !isWindow(hwnd) {
			return
		}
	}
	if pid := windowPID(hwnd); pid != 0 {
		_ = exec.Command("taskkill.exe", "/PID", fmt.Sprint(pid), "/T", "/F").Run()
	} else if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
