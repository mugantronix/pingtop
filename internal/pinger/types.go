// Package pinger probes ICMPv4 reachability for a list of targets.
//
// This build targets Windows only and pings via IcmpSendEcho
// (iphlpapi.dll) — see run_windows.go for why and how. This file
// holds the type definitions shared with main.go and internal/ui:
// StatsUpdate (what gets published per ping event) and Pinger (the
// per-target config struct), plus the small emit helper both would
// otherwise duplicate.
package pinger

import (
	"context"
	"time"
)

// StatsUpdate is one snapshot of a target's ping state, sent on the
// shared updates channel after each send/receive/error event. The UI
// keeps only the latest update per TargetID, so each value is a full
// replacement rather than an increment.
type StatsUpdate struct {
	TargetID string
	Sent     int64
	Recv     int64
	RTT      time.Duration // most recent successful sample; zero if N/A
	MinRTT   time.Duration // smallest RTT observed; zero until first reply
	AvgRTT   time.Duration // running mean over recv replies; zero until first reply
	MaxRTT   time.Duration // largest RTT observed; zero until first reply
	Jitter   time.Duration // RFC 3550 smoothed inter-packet jitter; zero until 2nd reply
	TTL      int           // TTL the most recent reply came back with; 0 if N/A
	// TimedOut marks THIS update as reporting a round with no reply
	// (request sent, nothing came back within the interval) — distinct
	// from LastErr, which is a real failure (DNS resolution, socket
	// error). RTT/MinRTT/AvgRTT/MaxRTT/TTL still carry the last known
	// good values when TimedOut is true; the UI uses this flag to
	// color those stale-but-still-shown values as critical rather than
	// silently displaying them as if the host just answered.
	TimedOut bool
	LastErr  error // sticky last error for display; nil on success
	Dropped  bool  // pinger has stopped; UI should remove this target
	// DownSince marks when this target entered a "down" state: more
	// than 5 consecutive failed rounds (timeouts or hard errors) with
	// no successful reply in between. The zero Time means "not
	// currently down". A single successful reply immediately clears
	// it back to zero, regardless of how many failures preceded it —
	// see run_windows.go's consecutiveFails/downSinceNano handling.
	DownSince time.Time
}

// Pinger runs a continuous ICMP echo loop against a single target and
// publishes StatsUpdates on Updates. Construct one per target. Run is
// implemented in run_windows.go.
type Pinger struct {
	ID       string
	Host     string
	Mode     Mode // kept for API compatibility with main.go; unused on this build (see mode.go)
	Interval time.Duration
	Size     int
	Drop     int // >0: drop target after this many sends with zero recvs
	Updates  chan<- StatsUpdate
}

// emit sends an update with bounded blocking. A full channel only
// stalls until ctx is cancelled; missing one update is fine since
// every update is a full snapshot.
func (p *Pinger) emit(ctx context.Context, u StatsUpdate) {
	select {
	case p.Updates <- u:
	case <-ctx.Done():
	}
}
