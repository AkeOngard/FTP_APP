//go:build !windows

package main

// trimWorkingSets has no counterpart on Linux and macOS, where the web view is managed by the system.
func trimWorkingSets() {}
