//go:build windows

package pinger

import (
	"fmt"
	"log"
	"os"
	"sync"
)

// debugLogger is a minimal, best-effort file logger used only to
// diagnose Windows-specific syscall failures during development
// (IcmpSendEcho errors, clipboard read errors) that are otherwise
// invisible — the TUI has no room to show a full error string, and
// there's no remote access to a tester's machine to attach a
// debugger. Enabled only when PINGTOP_DEBUG is set in the
// environment, so it's silent by default and never writes to a
// normal user's disk unasked.
//
// This is deliberately temporary scaffolding for tracking down the
// "err" / silent-paste-failure reports — safe to delete once the
// underlying bugs are found and fixed, at which point normal
// StatsUpdate.LastErr / the paste status banner are the intended way
// to surface problems to the user.
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
