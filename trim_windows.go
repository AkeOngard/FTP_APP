//go:build windows

package main

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procEmptyWorkingSet = windows.NewLazySystemDLL("psapi.dll").NewProc("EmptyWorkingSet")

// processTree returns root and every process descended from it: for this app, the WebView2
// browser, renderer, GPU and utility processes that make up most of its memory.
func processTree(root uint32) []uint32 {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return []uint32{root}
	}
	defer windows.CloseHandle(snap)

	children := map[uint32][]uint32{}
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		children[pe.ParentProcessID] = append(children[pe.ParentProcessID], pe.ProcessID)
	}

	seen := map[uint32]bool{root: true}
	out := []uint32{root}
	for i := 0; i < len(out); i++ { // breadth first; the seen set also guards against recycled-PID loops
		for _, c := range children[out[i]] {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	return out
}

// trimWorkingSets asks Windows to take back the memory pages this app and its WebView2 helpers are not
// using right now. Nothing is lost: pages that are touched again come back on demand, so only what the
// UI actually needs returns, which is far less than what start-up left behind.
func trimWorkingSets() {
	for _, pid := range processTree(uint32(os.Getpid())) {
		h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_QUERY_INFORMATION, false, pid)
		if err != nil {
			continue // a process we may not touch; skip it
		}
		procEmptyWorkingSet.Call(uintptr(h))
		windows.CloseHandle(h)
	}
}
