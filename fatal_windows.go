//go:build windows

package main

import "golang.org/x/sys/windows"

// showFatal tells the user why the app cannot start. A Windows GUI program has no console, so without
// a message box the app would just not open.
func showFatal(msg string) {
	text, err1 := windows.UTF16PtrFromString(msg)
	title, err2 := windows.UTF16PtrFromString("File Trans")
	if err1 != nil || err2 != nil {
		return
	}
	windows.MessageBox(0, text, title, windows.MB_OK|windows.MB_ICONERROR)
}
