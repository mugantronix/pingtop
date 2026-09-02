package ui

import (
	"fmt"
	"log"
	"os"
	"sync"
)

// debugLog is temporary diagnostic scaffolding for tracking down
// Windows-specific input quirks (this package's Ctrl+V paste
// investigation in particular) that are otherwise invisible — the TUI
// has no room to show raw key event data, and there's no remote
// access to a tester's machine. Enabled only when PINGTOP_DEBUG is
// set, so it's silent and writes nothing to disk by default.
//
// Deliberately separate from internal/pinger's identically-named
// helper: that one carries a //go:build windows tag since it wraps
// Windows-only syscalls, but key event logging is useful on any
// platform, so this copy stays unconstrained. Safe to delete once the
// paste-detection bug is found and fixed.
var (
	debugMu   sync.Mutex
	debugFile *os.File
)

func init() {
	if os.Getenv("PINGTOP_DEBUG") == "" {
		return
	}
	f, err := os.OpenFile("pingtop-debug.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return // best-effort; debugLog silently no-ops if debugFile is nil
	}
	debugFile = f
}

func debugLog(format string, args ...any) {
	debugMu.Lock()
	defer debugMu.Unlock()
	if debugFile == nil {
		return
	}
	log.New(debugFile, "", log.LstdFlags|log.Lmicroseconds).Print(fmt.Sprintf(format, args...))
}
