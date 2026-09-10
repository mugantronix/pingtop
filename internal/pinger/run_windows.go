//go:build windows

package pinger

import (
	"context"
	"fmt"
	"math"
	"net"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// Run blocks until ctx is cancelled, sending one echo per Interval and
// emitting a StatsUpdate after each send/receive cycle.
//
// Unlike the Unix implementation (run_unix.go), which uses a raw or
// dgram ICMP socket via pro-bing, Windows has no unprivileged raw
// socket path — opening one requires Administrator. Instead this
// calls IcmpSendEcho in iphlpapi.dll, the Windows API applications
// normally use for ICMP: it runs entirely in kernel space on behalf
// of the caller and needs no elevated privileges at all. The
// trade-off is a coarser API: IcmpSendEcho is synchronous (one
// blocking call per echo, no async send/recv callbacks) and reports
// RTT rounded to whole milliseconds, not sub-millisecond precision.
// That's a Windows API limitation, not something this code can work
// around, so jitter and sparkline resolution are correspondingly
// coarser on Windows than on Unix.
func (p *Pinger) Run(ctx context.Context) error {
	dst, err := resolveIPv4(p.Host)
	if err != nil {
		debugLog("resolveIPv4 id=%s host=%s err=%v", p.ID, p.Host, err)
		p.emit(ctx, StatsUpdate{TargetID: p.ID, LastErr: fmt.Errorf("resolve: %w", err)})
		return err
	}

	h, err := icmpCreateFile()
	if err != nil {
		debugLog("IcmpCreateFile id=%s err=%v", p.ID, err)
		p.emit(ctx, StatsUpdate{TargetID: p.ID, LastErr: fmt.Errorf("IcmpCreateFile: %w", err)})
		return err
	}
	debugLog("IcmpCreateFile id=%s handle=%v ok", p.ID, h)
	defer icmpCloseHandle(h)

	pCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var sent, recv atomic.Int64
	var prevRTT, jitter atomic.Int64
	var minRTT, sumRTT, maxRTT atomic.Int64
	var lastTTL atomic.Int32
	var lastTimedOut atomic.Bool
	var consecutiveFails atomic.Int64
	var downSinceNano atomic.Int64 // 0 = not currently down
	minRTT.Store(math.MaxInt64)

	// downThreshold matches the "quando falliscono più di 5 ping"
	// requirement literally: the target enters the down state on the
	// 6th consecutive failed round (5 fails is not yet "more than 5").
	const downThreshold = 5

	// markFailure records one failed round (timeout or hard error) and
	// flips the target into the down state once consecutiveFails
	// exceeds downThreshold. Idempotent about downSinceNano — it only
	// ever sets it once per outage, never bumping it forward on
	// subsequent failures within the same outage, so DownSince always
	// reflects when the outage actually started.
	markFailure := func() {
		n := consecutiveFails.Add(1)
		if n > downThreshold && downSinceNano.Load() == 0 {
			downSinceNano.Store(time.Now().UnixNano())
		}
	}
	// markSuccess clears both the streak and the down state the moment
	// a single reply comes back — "se il ping ritorna a funzionare si
	// azzera" doesn't require 5 consecutive successes, just one.
	markSuccess := func() {
		consecutiveFails.Store(0)
		downSinceNano.Store(0)
	}

	snapshot := func(rtt time.Duration, lastErr error) StatsUpdate {
		min := time.Duration(minRTT.Load())
		if min == time.Duration(math.MaxInt64) {
			min = 0
		}
		var avg time.Duration
		if n := recv.Load(); n > 0 {
			avg = time.Duration(sumRTT.Load() / n)
		}
		var downSince time.Time
		if ns := downSinceNano.Load(); ns != 0 {
			downSince = time.Unix(0, ns)
		}
		return StatsUpdate{
			TargetID: p.ID,
			Sent:     sent.Load(),
			Recv:     recv.Load(),
			RTT:      rtt,
			MinRTT:   min,
			AvgRTT:   avg,
			MaxRTT:   time.Duration(maxRTT.Load()),
			Jitter:   time.Duration(jitter.Load()),
			TTL:      int(lastTTL.Load()),
			// TimedOut reflects the outcome of the MOST RECENT
			// completed round, not "was this specific snapshot call
			// triggered by a timeout". Without this, the pre-send
			// snapshot emitted at the top of the next tick (before
			// that tick's icmpEcho has even run) would default
			// TimedOut back to false and immediately clear the
			// critical coloring the previous timeout just set — the
			// row would flash red for one frame then revert to green,
			// which is the bug this field exists to prevent. See
			// TimedOut's doc on StatsUpdate.
			TimedOut:  lastTimedOut.Load(),
			LastErr:   lastErr,
			DownSince: downSince,
		}
	}

	payload := make([]byte, p.Size)
	// timeoutMS bounds a single IcmpSendEcho call. It's independent of
	// the ping cadence (Interval) — this is "how long to wait for this
	// one reply", not "how often to send". Cap it comfortably under
	// Interval so a slow/unreachable host doesn't cause echoes to pile
	// up, but give it at least a full second so a healthy but slightly
	// slow WAN hop isn't misreported as loss.
	timeoutMS := uint32(p.Interval.Milliseconds())
	if timeoutMS < 1000 {
		timeoutMS = 1000
	}

	ticker := time.NewTicker(p.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-pCtx.Done():
			return nil
		case <-ticker.C:
		}

		n := sent.Add(1)
		if p.Drop > 0 && n >= int64(p.Drop) && recv.Load() == 0 {
			u := snapshot(0, nil)
			u.Dropped = true
			p.emit(pCtx, u)
			cancel()
			return nil
		}
		// Mirror run_unix.go: emit with sent-1 so loss% reflects the
		// settled outcome of prior packets, not the in-flight one.
		preSend := snapshot(0, nil)
		preSend.Sent = n - 1
		p.emit(pCtx, preSend)

		rtt, ttl, err := icmpEcho(h, dst, payload, timeoutMS)
		if err != nil {
			debugLog("icmpEcho id=%s dst=%#08x replySize=%d err=%v", p.ID, dst, int(unsafe.Sizeof(icmpEchoReply{}))+len(payload)+8, err)
			lastTimedOut.Store(false)
			markFailure()
			p.emit(pCtx, snapshot(0, err))
			continue
		}
		if rtt < 0 {
			// Timeout / unreachable / TTL expired: no reply this round.
			// Not a LastErr-worthy failure (mirrors run_unix.go's
			// OnRecvError deadline-exceeded suppression) — but the UI
			// still needs to know THIS round had no reply, so it can
			// mark the row critical instead of silently continuing to
			// show a stale RTT as if the host just answered. See
			// TimedOut's doc on StatsUpdate.
			lastTimedOut.Store(true)
			markFailure()
			p.emit(pCtx, snapshot(0, nil))
			continue
		}

		lastTimedOut.Store(false)
		markSuccess()
		lastTTL.Store(int32(ttl))
		recv.Add(1)
		sumRTT.Add(int64(rtt))
		if cur := minRTT.Load(); int64(rtt) < cur {
			minRTT.Store(int64(rtt))
		}
		if cur := maxRTT.Load(); int64(rtt) > cur {
			maxRTT.Store(int64(rtt))
		}
		prev := time.Duration(prevRTT.Swap(int64(rtt)))
		if prev > 0 {
			d := rtt - prev
			if d < 0 {
				d = -d
			}
			j := time.Duration(jitter.Load())
			jitter.Store(int64(j + (d-j)/16))
		}
		p.emit(pCtx, snapshot(rtt, nil))
	}
}

// resolveIPv4 resolves host to its first IPv4 address as a
// big-endian uint32, the form IcmpSendEcho expects for its
// destination address. IPv6 targets aren't supported by IcmpSendEcho
// (Icmp6SendEcho2 is the separate, more involved API for that) — out
// of scope for this fix, which is specifically about letting
// unprivileged Windows users ping ordinary IPv4/hostname targets.
func resolveIPv4(host string) (uint32, error) {
	ips, err := net.LookupIP(host)
	if err != nil {
		return 0, err
	}
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			return uint32(v4[0]) | uint32(v4[1])<<8 | uint32(v4[2])<<16 | uint32(v4[3])<<24, nil
		}
	}
	return 0, fmt.Errorf("no IPv4 address found for %q", host)
}

// --- IcmpSendEcho syscall bindings ---
//
// Bound directly via syscall.NewLazyDLL rather than golang.org/x/sys/windows
// so this file's only dependency is the standard library — the whole
// point of using IcmpSendEcho is to avoid needing anything beyond
// what every Windows install already has in System32.

var (
	modIphlpapi        = syscall.NewLazyDLL("iphlpapi.dll")
	procIcmpCreateFile  = modIphlpapi.NewProc("IcmpCreateFile")
	procIcmpCloseHandle = modIphlpapi.NewProc("IcmpCloseHandle")
	procIcmpSendEcho    = modIphlpapi.NewProc("IcmpSendEcho")
)

// icmpEchoReply mirrors Windows' ICMP_ECHO_REPLY struct (icmpapi.h)
// in full. An earlier version of this struct only had the first three
// fields (Address/Status/RoundTripTime) on the theory that the
// trailing fields weren't needed for reading the result — but
// IcmpSendEcho validates the caller-supplied ReplySize against the
// FULL struct size plus request data plus slack, and rejects the call
// with IP_GENERAL_FAILURE (11050) if the buffer is too small to hold
// what it intends to write. Getting this struct's size right is load
// bearing, not just descriptive: replySize below is computed from
// unsafe.Sizeof(icmpEchoReply{}), so an undersized struct undersizes
// the buffer and the call fails outright — which is exactly what was
// happening (every call, on every Windows build, regardless of
// privilege level, since this was a struct-layout bug, not a
// permissions one).
type icmpEchoReply struct {
	Address       uint32
	Status        uint32
	RoundTripTime uint32
	DataSize      uint16
	Reserved      uint16
	Data          uintptr
	Options       ipOptionInformation
}

// ipOptionInformation mirrors IP_OPTION_INFORMATION (ipexport.h), the
// struct IcmpSendEcho's RequestOptions parameter points to.
//
// Passing NULL for RequestOptions (which the Windows docs describe as
// valid — "if NULL, default option is used") triggers
// IP_GENERAL_FAILURE (11050) for non-elevated callers on modern
// Windows; passing a zeroed-but-non-nil struct with just Ttl filled
// in is the documented workaround (also matches what ping.exe and
// other unprivileged callers do under the hood). OptionsData is left
// nil — no IP options beyond TTL are needed for a plain echo.
type ipOptionInformation struct {
	Ttl         byte
	Tos         byte
	Flags       byte
	OptionsSize byte
	OptionsData uintptr
}

func icmpCreateFile() (syscall.Handle, error) {
	r, _, err := procIcmpCreateFile.Call()
	h := syscall.Handle(r)
	if h == syscall.InvalidHandle {
		return 0, err
	}
	return h, nil
}

func icmpCloseHandle(h syscall.Handle) {
	_, _, _ = procIcmpCloseHandle.Call(uintptr(h))
}

// icmpStatusSuccess is IP_SUCCESS from ipexport.h — the Status value
// in ICMP_ECHO_REPLY on a successful round trip.
const icmpStatusSuccess = 0

// defaultTTL matches the TTL Windows' own ping.exe uses by default.
const defaultTTL = 128

// icmpEcho sends one echo to dst (a big-endian IPv4 address, see
// resolveIPv4) and blocks up to timeoutMS for the reply. Returns the
// RTT and the TTL the reply came back with on success, -1 RTT with a
// nil error on timeout/unreachable (the caller treats that as "no
// reply this round", matching run_unix.go's handling of
// deadline-exceeded), or a non-nil error for anything IcmpSendEcho
// itself couldn't attempt (bad handle, out of resources).
func icmpEcho(h syscall.Handle, dst uint32, payload []byte, timeoutMS uint32) (rtt time.Duration, ttl int, err error) {
	// Reply buffer must fit one ICMP_ECHO_REPLY plus the echoed request
	// data plus 8 extra bytes, per the IcmpSendEcho documentation's
	// sizing guidance (room for a possible ICMP error message body).
	replySize := int(unsafe.Sizeof(icmpEchoReply{})) + len(payload) + 8
	reply := make([]byte, replySize)

	var payloadPtr uintptr
	if len(payload) > 0 {
		payloadPtr = uintptr(unsafe.Pointer(&payload[0]))
	}

	opts := ipOptionInformation{Ttl: defaultTTL}

	r, _, callErr := procIcmpSendEcho.Call(
		uintptr(h),
		uintptr(dst),
		payloadPtr,
		uintptr(len(payload)),
		uintptr(unsafe.Pointer(&opts)),
		uintptr(unsafe.Pointer(&reply[0])),
		uintptr(len(reply)),
		uintptr(timeoutMS),
	)
	if r == 0 {
		// IcmpSendEcho returns 0 both for "no reply within timeout"
		// (callErr == ERROR_SUCCESS-ish transient codes) and for real
		// failures. GetLastError distinguishes them; timeouts and
		// unreachable-host codes are treated as "no reply" rather
		// than surfaced errors, matching the Unix path's handling.
		if isIcmpTimeoutErr(callErr) {
			return -1, 0, nil
		}
		return 0, 0, fmt.Errorf("IcmpSendEcho: %w", callErr)
	}

	rep := (*icmpEchoReply)(unsafe.Pointer(&reply[0]))
	if rep.Status != icmpStatusSuccess {
		// Non-zero Status with r>0 (replies returned) means the
		// device answered but reported an ICMP error (TTL expired,
		// dest unreachable, etc.) rather than a true echo reply.
		// Treat as "no reply" like a timeout.
		return -1, 0, nil
	}
	// rep.Options.Ttl is the TTL the reply itself carried — i.e. what
	// the replying host's IP stack set on its outbound packet, already
	// decremented by any hops back to us. That's the conventional
	// "ping TTL" value tools like ping.exe display, distinct from the
	// defaultTTL we set as our own outbound request's TTL above.
	return time.Duration(rep.RoundTripTime) * time.Millisecond, int(rep.Options.Ttl), nil
}

// isIcmpTimeoutErr reports whether err corresponds to one of the
// IP_* status codes IcmpSendEcho signals via GetLastError when a
// request completes without a successful reply (timeout, TTL
// expired, host/net unreachable, etc.), as opposed to a genuine call
// failure. These are IP_* constants from ipexport.h, offset by
// IP_STATUS_BASE (11000) the way FormatMessage / GetLastError expose
// them for this API.
func isIcmpTimeoutErr(err error) bool {
	errno, ok := err.(syscall.Errno)
	if !ok {
		return false
	}
	switch uintptr(errno) {
	case 11010, // IP_REQ_TIMED_OUT
		11002, // IP_DEST_NET_UNREACHABLE
		11003, // IP_DEST_HOST_UNREACHABLE
		11004, // IP_DEST_PROT_UNREACHABLE
		11005, // IP_DEST_PORT_UNREACHABLE
		11013, // IP_TTL_EXPIRED_TRANSIT
		11014: // IP_TTL_EXPIRED_REASSEM
		return true
	}
	return false
}
