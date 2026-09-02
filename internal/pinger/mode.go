package pinger

// This file is a thin compatibility shim. The original (Linux/macOS)
// implementation used DetectMode to choose between an unprivileged
// SOCK_DGRAM ICMP socket and a privileged raw one. This Windows-only
// fork uses IcmpSendEcho (see run_windows.go) instead, which needs no
// such choice — it works for any unprivileged user by design. Mode
// and DetectMode are kept only so main.go's call site doesn't need to
// change; Mode carries no information here and can be dropped
// entirely if main.go is ever simplified further.

// Mode is a no-op placeholder — IcmpSendEcho doesn't distinguish
// privilege levels the way raw ICMP sockets do.
type Mode int

// DetectMode always succeeds: IcmpSendEcho works for any Windows user
// without elevation, so there's nothing to probe.
func DetectMode() (Mode, error) {
	return Mode(0), nil
}
