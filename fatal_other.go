//go:build !windows

package main

import (
	"fmt"
	"os"
)

// showFatal tells the user why the app cannot start.
func showFatal(msg string) { fmt.Fprintln(os.Stderr, "FTP App:", msg) }
