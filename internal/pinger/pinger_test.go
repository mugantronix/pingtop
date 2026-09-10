//go:build windows

package pinger

import (
	"context"
	"testing"
	"time"
)

func TestDetectModeAlwaysSucceeds(t *testing.T) {
	if _, err := DetectMode(); err != nil {
		t.Errorf("DetectMode should never fail on this build, got: %v", err)
	}
}

func TestResolveIPv4(t *testing.T) {
	addr, err := resolveIPv4("127.0.0.1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 127.0.0.1 packed little-endian: byte0=127, byte1=0, byte2=0, byte3=1.
	want := uint32(127) | uint32(0)<<8 | uint32(0)<<16 | uint32(1)<<24
	if addr != want {
		t.Errorf("got %#x, want %#x", addr, want)
	}
}

func TestResolveIPv4RejectsIPv6OnlyHost(t *testing.T) {
	// ::1 has no IPv4 form, so resolveIPv4 (which filters via To4())
	// must reject it rather than silently mishandling it.
	if _, err := resolveIPv4("::1"); err == nil {
		t.Error("expected an error resolving an IPv6-only literal, got nil")
	}
}

// TestPingerLoopback is an integration smoke test: ping 127.0.0.1 and
// confirm we see at least one successful reply (Recv > 0, LastErr
// nil) within the deadline. Windows loopback RTT is commonly 0ms
// (IcmpSendEcho reports whole milliseconds and loopback is normally
// sub-millisecond), so this checks Recv/LastErr rather than RTT > 0.
func TestPingerLoopback(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test; skipped in -short")
	}

	updates := make(chan StatsUpdate, 16)
	p := &Pinger{
		ID:       "127.0.0.1",
		Host:     "127.0.0.1",
		Interval: 200 * time.Millisecond,
		Size:     24,
		Updates:  updates,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	deadline := time.After(2500 * time.Millisecond)
	for {
		select {
		case u := <-updates:
			if u.Recv > 0 && u.LastErr == nil {
				cancel()
				<-done
				return
			}
		case <-deadline:
			cancel()
			<-done
			t.Fatal("no successful reply from 127.0.0.1 within deadline")
		}
	}
}

// TestPingerTimedOutOnUnreachableHost is an integration smoke test:
// ping an address in the TEST-NET-1 documentation range (RFC 5737,
// 192.0.2.0/24 — reserved for examples, guaranteed unroutable) and
// confirm the pinger emits a StatsUpdate with TimedOut=true instead
// of staying silent. This is what run_windows.go's timeout path is
// for — see TimedOut's doc on StatsUpdate for why silently dropping
// the round instead of reporting it was the bug.
//
// It also confirms TimedOut STAYS true across several consecutive
// updates (the pre-send snapshot for the next round included) rather
// than flashing true for one message and reverting to false — that
// flash-then-revert was a regression: the pre-send snapshot at the
// top of each tick used to default TimedOut back to its zero value
// before that tick's own icmpEcho had even run, undoing the previous
// round's critical state for one frame before the row went red again.
func TestPingerTimedOutOnUnreachableHost(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test; skipped in -short")
	}

	updates := make(chan StatsUpdate, 16)
	p := &Pinger{
		ID:       "192.0.2.1",
		Host:     "192.0.2.1",
		Interval: 200 * time.Millisecond,
		Size:     24,
		Updates:  updates,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	deadline := time.After(4500 * time.Millisecond)
	var timedOutSeen, sawFalseAfterTrue int
	for {
		select {
		case u := <-updates:
			if u.TimedOut {
				timedOutSeen++
			} else if timedOutSeen > 0 {
				// Every unrouted host is guaranteed to also be behind
				// on its own DNS resolve step never happening here
				// (Host is already a literal IP), so once we've seen
				// TimedOut=true, EVERY subsequent update until process
				// exit should also be TimedOut=true — there's no way
				// for 192.0.2.1 to suddenly reply mid-test.
				sawFalseAfterTrue++
			}
			if timedOutSeen >= 3 {
				cancel()
				<-done
				if sawFalseAfterTrue > 0 {
					t.Errorf("TimedOut flickered false after being true %d time(s) — expected it to stay true across consecutive updates for a consistently unreachable host", sawFalseAfterTrue)
				}
				return
			}
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("expected at least 3 TimedOut=true updates from an unroutable host within deadline, got %d", timedOutSeen)
		}
	}
}

// TestPingerDownSinceAfterFiveConsecutiveFailures pings an unroutable
// host (same TEST-NET-1 address as the TimedOut test above) and
// confirms DownSince stays zero through the first 5 consecutive
// failed rounds, then becomes non-zero starting with the 6th — "più
// di 5" (more than 5) failures, matching the requirement literally.
// Also confirms DownSince, once set, doesn't reset itself on further
// failures (it should mark when the outage STARTED, not keep moving
// forward).
func TestPingerDownSinceAfterFiveConsecutiveFailures(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test; skipped in -short")
	}

	updates := make(chan StatsUpdate, 32)
	p := &Pinger{
		ID:       "192.0.2.1",
		Host:     "192.0.2.1",
		Interval: 200 * time.Millisecond,
		Size:     24,
		Updates:  updates,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	deadline := time.After(35 * time.Second)
	var failedRounds int
	var firstDownSince time.Time
	for {
		select {
		case u := <-updates:
			if !u.TimedOut {
				continue // pre-send snapshots and any other non-outcome message
			}
			failedRounds++
			switch {
			case failedRounds <= 5:
				if !u.DownSince.IsZero() {
					t.Errorf("round %d: expected DownSince still zero (only %d failures so far), got %v", failedRounds, failedRounds, u.DownSince)
				}
			case failedRounds == 6:
				if u.DownSince.IsZero() {
					t.Error("round 6: expected DownSince to be set once failures exceed 5, still zero")
				}
				firstDownSince = u.DownSince
			default:
				if u.DownSince.IsZero() {
					t.Errorf("round %d: DownSince reverted to zero while still failing", failedRounds)
				} else if !u.DownSince.Equal(firstDownSince) {
					t.Errorf("round %d: DownSince moved from %v to %v — should stay pinned to when the outage started", failedRounds, firstDownSince, u.DownSince)
				}
			}
			if failedRounds >= 8 {
				cancel()
				<-done
				return
			}
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("only saw %d failed rounds within deadline, needed at least 8", failedRounds)
		}
	}
}
