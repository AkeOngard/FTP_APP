//go:build windows

package main

import (
	"os"
	"os/exec"
	"testing"
)

func TestProcessTreeFindsDescendants(t *testing.T) {
	// A grandchild: cmd starts ping, which keeps running while we look.
	cmd := exec.Command("cmd", "/c", "ping", "-n", "6", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a child process: %v", err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()

	self := uint32(os.Getpid())
	tree := processTree(self)
	if tree[0] != self {
		t.Fatalf("the tree must start with the root process, got %v", tree)
	}
	found := false
	for _, pid := range tree {
		if pid == uint32(cmd.Process.Pid) {
			found = true
		}
	}
	if !found {
		t.Errorf("child %d missing from process tree %v", cmd.Process.Pid, tree)
	}
}

func TestTrimWorkingSetsDoesNotDisturbTheProcess(t *testing.T) {
	data := make([]byte, 64<<20)
	for i := range data {
		data[i] = byte(i)
	}
	trimWorkingSets() // must not crash, and our own memory must still read back correctly afterwards
	for i := 0; i < len(data); i += 4096 {
		if data[i] != byte(i) {
			t.Fatalf("memory changed after trimming at offset %d", i)
		}
	}
}
