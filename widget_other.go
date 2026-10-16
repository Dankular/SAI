//go:build !windows

package main

import "errors"

// The desktop widget is Windows-only (it needs the Windows desktop layer).

type widgetRuntime struct{}

func (a *app) startWidget() error {
	return errors.New("the desktop widget is only available on Windows")
}
func (a *app) stopWidget()         {}
func (a *app) widgetHealthy() bool { return false }
func (a *app) applyWidgetLayout()  {}
