package ui

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/guerrieroriccardo/pingtop/internal/pinger"
)

// newTestModel is New() with sensible test defaults: no cmds channel
// (nil is safe — Update guards every send with a nil check) and a
// generous maxHosts so paste tests don't need to think about caps
// unless they're specifically testing the cap.
func newTestModel(ids []string, updates <-chan pinger.StatsUpdate, keepDropped, colorize bool) Model {
	return New(ids, updates, keepDropped, colorize, nil, 256)
}

func TestFormatRTT(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    pinger.StatsUpdate
		want string
	}{
		{"no data", pinger.StatsUpdate{}, "—"},
		{"with rtt", pinger.StatsUpdate{Recv: 1, RTT: 2*time.Millisecond + 500*time.Microsecond}, "2.5ms"},
		{"err sticky", pinger.StatsUpdate{LastErr: errors.New("boom")}, "err"},
		{"rtt overrides err", pinger.StatsUpdate{Recv: 1, RTT: time.Millisecond, LastErr: errors.New("boom")}, "1ms"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatRTT(tc.s); got != tc.want {
				t.Errorf("formatRTT = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFormatJitter(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    pinger.StatsUpdate
		want string
	}{
		{"no data", pinger.StatsUpdate{}, "—"},
		{"with jitter", pinger.StatsUpdate{Recv: 2, Jitter: 750 * time.Microsecond}, "0.75ms"},
		{"jitter capped at 2 decimals", pinger.StatsUpdate{Recv: 2, Jitter: 1234567 * time.Nanosecond}, "1.23ms"},
		{"err has no effect", pinger.StatsUpdate{LastErr: errors.New("boom")}, "—"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatJitter(tc.s); got != tc.want {
				t.Errorf("formatJitter = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFormatLoss(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    pinger.StatsUpdate
		want string
	}{
		{"no sends yet", pinger.StatsUpdate{}, "—"},
		{"all received", pinger.StatsUpdate{Sent: 10, Recv: 10}, "0.0%"},
		{"half lost", pinger.StatsUpdate{Sent: 10, Recv: 5}, "50.0%"},
		{"all lost", pinger.StatsUpdate{Sent: 5, Recv: 0}, "100.0%"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatLoss(tc.s); got != tc.want {
				t.Errorf("formatLoss = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFormatSentLost(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    pinger.StatsUpdate
		want string
	}{
		{"no data", pinger.StatsUpdate{}, "—"},
		{"all received", pinger.StatsUpdate{Sent: 10, Recv: 10}, "10/0"},
		{"two lost", pinger.StatsUpdate{Sent: 10, Recv: 8}, "10/2"},
		{"recv exceeds sent clamps", pinger.StatsUpdate{Sent: 5, Recv: 6}, "5/0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatSentLost(tc.s); got != tc.want {
				t.Errorf("formatSentLost = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildRowsInitial(t *testing.T) {
	rows := buildRows([]string{"1.1.1.1", "8.8.8.8"}, nil, nil, styler{}, sparkWidth, false)
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	if rows[0][0] != "1.1.1.1" || rows[1][0] != "8.8.8.8" {
		t.Errorf("rows lost their order: %v", rows)
	}
	for _, r := range rows {
		if len(r) != 10 {
			t.Errorf("expected 10 cells, got %d in %v", len(r), r)
			continue
		}
		for i := 1; i <= 8; i++ {
			if r[i] != "—" {
				t.Errorf("expected placeholder at index %d, got %q in %v", i, r[i], r)
			}
		}
	}
}

func TestRenderTableShowsLastRowsAtMaxOffset(t *testing.T) {
	updates := make(chan pinger.StatsUpdate)
	ids := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	m := newTestModel(ids, updates, false, false)
	m.termWidth = 200 // wide enough for all columns
	m.termHeight = 6  // 1 help + 2 header = 3 data rows visible
	m.offset = m.maxOffset()

	out := m.renderTable()
	// At maxOffset the last visible chunk must include "h" — the regression
	// was that lipgloss/table's overflow ellipsis swallowed the bottom row.
	if !strings.Contains(out, "h") {
		t.Errorf("expected last row 'h' visible at maxOffset, got:\n%s", out)
	}
	if !strings.Contains(out, "g") {
		t.Errorf("expected penultimate row 'g' visible at maxOffset, got:\n%s", out)
	}
}

func TestScrollWithinBounds(t *testing.T) {
	updates := make(chan pinger.StatsUpdate)
	m := newTestModel([]string{"a", "b", "c", "d", "e"}, updates, false, false)
	m.termHeight = 5 // 5 lines total: 1 help + 2 header = 2 available data rows.

	// First down arrow scrolls; further presses cap at maxOffset.
	for i := 0; i < 10; i++ {
		mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
		m = mm.(Model)
	}
	if m.offset != m.maxOffset() {
		t.Errorf("expected offset to clamp at maxOffset=%d, got %d", m.maxOffset(), m.offset)
	}

	// Up arrow walks it back to 0 and stays there.
	for i := 0; i < 20; i++ {
		mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})
		m = mm.(Model)
	}
	if m.offset != 0 {
		t.Errorf("expected offset to clamp at 0 after up-spam, got %d", m.offset)
	}
}

func TestUpdateApplyingStatsMsg(t *testing.T) {
	updates := make(chan pinger.StatsUpdate, 4)
	m := newTestModel([]string{"1.1.1.1", "8.8.8.8"}, updates, false, false)

	mm, _ := m.Update(statsMsg{
		TargetID: "1.1.1.1",
		Sent:     4,
		Recv:     3,
		RTT:      5 * time.Millisecond,
		MinRTT:   1 * time.Millisecond,
		AvgRTT:   3 * time.Millisecond,
		MaxRTT:   7 * time.Millisecond,
	})
	got := mm.(Model).stats["1.1.1.1"]
	if got.RTT != 5*time.Millisecond || got.Sent != 4 || got.Recv != 3 {
		t.Errorf("stats not stored as expected: %+v", got)
	}
	if got.MinRTT != 1*time.Millisecond || got.AvgRTT != 3*time.Millisecond || got.MaxRTT != 7*time.Millisecond {
		t.Errorf("min/avg/max not stored: %+v", got)
	}

	view := mm.(Model).View()
	for _, want := range []string{"5ms", "1ms", "3.00ms", "7ms", "25.0%"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q:\n%s", want, view)
		}
	}
}

func TestFormatDur(t *testing.T) {
	for _, tc := range []struct {
		name         string
		d            time.Duration
		recv         int64
		avgPrecision bool
		want         string
	}{
		{"no recv yet", 5 * time.Millisecond, 0, false, "—"},
		{"zero duration with recv", 0, 1, false, "0ms"},
		{"sub-ms rounds to fractional ms", 750 * time.Microsecond, 1, false, "0.75ms"},
		{"whole ms", 5 * time.Millisecond, 1, false, "5ms"},
		{"fractional ms", 2*time.Millisecond + 500*time.Microsecond, 1, false, "2.5ms"},
		{"avg capped at 2 decimals", 1234567 * time.Nanosecond, 1, true, "1.23ms"},
		{"avg zero still shows 2 decimals", 0, 1, true, "0.00ms"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatDur(tc.d, tc.recv, tc.avgPrecision); got != tc.want {
				t.Errorf("formatDur(%v, %d, %v) = %q, want %q", tc.d, tc.recv, tc.avgPrecision, got, tc.want)
			}
		})
	}
}

func TestFormatMsNeverFallsBackToSeconds(t *testing.T) {
	// Regression guard: time.Duration.String() renders an exact-zero
	// duration as "0s" (seconds), not "0ms" — formatMs must never do
	// that, since every duration in this table (RTT/jitter/min/avg/max)
	// is always meant to read in milliseconds.
	if got := formatMs(0); got != "0ms" {
		t.Errorf("formatMs(0) = %q, want %q", got, "0ms")
	}
}

func TestFormatMsHasNoSpaceBeforeUnit(t *testing.T) {
	for _, d := range []time.Duration{0, 5 * time.Millisecond, 750 * time.Microsecond, 123 * time.Millisecond} {
		got := formatMs(d)
		if strings.Contains(got, " ") {
			t.Errorf("formatMs(%v) = %q, expected no space before the unit", d, got)
		}
	}
}

func TestFormatMsPrecisionCapsDecimals(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{0, "0.00ms"},
		{5 * time.Millisecond, "5.00ms"},
		{1234567 * time.Nanosecond, "1.23ms"},
		{1236000 * time.Nanosecond, "1.24ms"}, // rounds, doesn't truncate
	} {
		if got := formatMsPrecision(tc.d, 2); got != tc.want {
			t.Errorf("formatMsPrecision(%v, 2) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestUpdateRemovesDroppedTarget(t *testing.T) {
	updates := make(chan pinger.StatsUpdate, 4)
	m := newTestModel([]string{"1.1.1.1", "8.8.8.8", "9.9.9.9"}, updates, false, false)

	mm, _ := m.Update(statsMsg{TargetID: "8.8.8.8", Dropped: true})
	out := mm.(Model)

	if len(out.order) != 2 || out.order[0] != "1.1.1.1" || out.order[1] != "9.9.9.9" {
		t.Errorf("order should be [1.1.1.1 9.9.9.9], got %v", out.order)
	}
	if _, ok := out.stats["8.8.8.8"]; ok {
		t.Errorf("stats for dropped target should be removed")
	}
	view := out.View()
	if strings.Contains(view, "8.8.8.8") {
		t.Errorf("view should not show dropped target:\n%s", view)
	}
	if !strings.Contains(view, "1.1.1.1") || !strings.Contains(view, "9.9.9.9") {
		t.Errorf("view should still show survivors:\n%s", view)
	}
}

func TestUpdateQuitOnKey(t *testing.T) {
	updates := make(chan pinger.StatsUpdate)
	m := newTestModel([]string{"1.1.1.1"}, updates, false, false)

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd == nil {
		t.Fatal("q should produce a Cmd")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("q should produce tea.QuitMsg, got %T", cmd())
	}
}

func TestUpdateQuitOnClosedChannel(t *testing.T) {
	updates := make(chan pinger.StatsUpdate)
	close(updates)
	m := newTestModel([]string{"1.1.1.1"}, updates, false, false)

	cmd := m.Init()
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("closed channel should produce tea.QuitMsg, got %T", cmd())
	}
}

func TestFilterMatchesSubstring(t *testing.T) {
	updates := make(chan pinger.StatsUpdate)
	m := newTestModel([]string{"1.1.1.1", "8.8.8.8", "192.168.1.10"}, updates, false, false)

	m.filter = "8.8"
	v := m.visibleIDs()
	if len(v) != 1 || v[0] != "8.8.8.8" {
		t.Errorf("expected [8.8.8.8] for filter %q, got %v", m.filter, v)
	}

	m.filter = "."
	v = m.visibleIDs()
	if len(v) != 3 {
		t.Errorf("expected all 3 to match filter %q, got %v", m.filter, v)
	}

	m.filter = "xyz"
	v = m.visibleIDs()
	if len(v) != 0 {
		t.Errorf("expected no matches for filter %q, got %v", m.filter, v)
	}
}

func TestFilterCaseInsensitive(t *testing.T) {
	updates := make(chan pinger.StatsUpdate)
	m := newTestModel([]string{"host-A.example", "HOST-b.example"}, updates, false, false)

	m.filter = "host-a"
	v := m.visibleIDs()
	if len(v) != 1 || v[0] != "host-A.example" {
		t.Errorf("expected [host-A.example] for filter %q, got %v", m.filter, v)
	}
}

func TestFormatSparkEmpty(t *testing.T) {
	got := formatSpark(nil, sparkWidth, false)
	if got != strings.Repeat(" ", sparkWidth) {
		t.Errorf("empty history should render as %d spaces, got %q", sparkWidth, got)
	}
}

func TestFormatSparkAllEqual(t *testing.T) {
	h := []time.Duration{10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond}
	got := formatSpark(h, sparkWidth, false)
	mid := string(sparkBarsUnicode[len(sparkBarsUnicode)/2])
	// Three middle bars, padded on the left to sparkWidth.
	want := strings.Repeat(" ", sparkWidth-3) + strings.Repeat(mid, 3)
	if got != want {
		t.Errorf("equal samples should all map to middle bar\n got=%q\nwant=%q", got, want)
	}
}

func TestFormatSparkScalesMinMax(t *testing.T) {
	h := []time.Duration{1 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond}
	got := formatSpark(h, sparkWidth, false)
	runes := []rune(got)
	// Last three runes are the data; min should be first bar, max should be last bar.
	last3 := runes[len(runes)-3:]
	if last3[0] != sparkBarsUnicode[0] {
		t.Errorf("min sample should map to %c, got %c", sparkBarsUnicode[0], last3[0])
	}
	if last3[2] != sparkBarsUnicode[len(sparkBarsUnicode)-1] {
		t.Errorf("max sample should map to %c, got %c", sparkBarsUnicode[len(sparkBarsUnicode)-1], last3[2])
	}
}

func TestFormatSparkRespectsWidth(t *testing.T) {
	got := formatSpark(nil, 50, false)
	if got != strings.Repeat(" ", 50) {
		t.Errorf("empty history at width=50 should render as 50 spaces, got %d chars", len([]rune(got)))
	}
}

func TestAppendHistoryRingBuffer(t *testing.T) {
	h := make(map[string][]time.Duration)
	for i := 0; i < maxSparkWidth+5; i++ {
		appendHistory(h, "x", time.Duration(i)*time.Millisecond)
	}
	if len(h["x"]) != maxSparkWidth {
		t.Errorf("history should cap at %d samples, got %d", maxSparkWidth, len(h["x"]))
	}
	// The oldest 5 samples should have been evicted; the buffer's first
	// sample should be sample #5 (zero-indexed).
	if h["x"][0] != 5*time.Millisecond {
		t.Errorf("oldest sample should be 5ms, got %v", h["x"][0])
	}
}

func TestEffectiveSparkWidthClaimsSlack(t *testing.T) {
	// All 10 columns visible total to 28+10+10+10+10+10+8+12+6+(20+2)=126.
	// At termWidth=166 there are 40 chars of slack — spark should claim it.
	got := effectiveSparkWidth(166)
	want := sparkWidth + (166 - 126)
	if got != want {
		t.Errorf("at termWidth=166 spark should be %d, got %d", want, got)
	}
}

func TestEffectiveSparkWidthCapped(t *testing.T) {
	got := effectiveSparkWidth(10_000)
	if got != maxSparkWidth {
		t.Errorf("very wide terminal should cap spark at %d, got %d", maxSparkWidth, got)
	}
}

func TestEffectiveSparkWidthDefaultWhenNoSlack(t *testing.T) {
	// At termWidth=126 the columns exactly fill: no slack.
	got := effectiveSparkWidth(126)
	if got != sparkWidth {
		t.Errorf("at termWidth=126 (exact fit) spark should be default %d, got %d", sparkWidth, got)
	}
}

func TestEffectiveSparkWidthDefaultWhenSparkHidden(t *testing.T) {
	// At termWidth=70 the SPARK column gets hidden by the tier algorithm.
	got := effectiveSparkWidth(70)
	if got != sparkWidth {
		t.Errorf("when SPARK is hidden spark should fall back to default %d, got %d", sparkWidth, got)
	}
}

func TestUpdateAppendsHistoryOnRTT(t *testing.T) {
	updates := make(chan pinger.StatsUpdate, 4)
	m := newTestModel([]string{"1.1.1.1"}, updates, false, false)

	mm, _ := m.Update(statsMsg{TargetID: "1.1.1.1", Sent: 1, Recv: 1, RTT: 3 * time.Millisecond})
	out := mm.(Model)
	if len(out.history["1.1.1.1"]) != 1 || out.history["1.1.1.1"][0] != 3*time.Millisecond {
		t.Errorf("expected one 3ms sample, got %v", out.history["1.1.1.1"])
	}

	// An RTT=0 message (OnSend snapshot) should NOT append.
	mm, _ = out.Update(statsMsg{TargetID: "1.1.1.1", Sent: 2, Recv: 1, RTT: 0})
	out = mm.(Model)
	if len(out.history["1.1.1.1"]) != 1 {
		t.Errorf("RTT=0 message should not append, got %v", out.history["1.1.1.1"])
	}
}

// TestTimedOutKeepsStaleValueButTurnsCritical exercises the full
// reply → timeout → reply cycle: a timeout must keep showing the last
// known RTT/Jitter (not blank them out) but mark them critical, and a
// subsequent real reply must clear that critical state.
func TestTimedOutKeepsStaleValueButTurnsCritical(t *testing.T) {
	updates := make(chan pinger.StatsUpdate, 4)
	m := newTestModel([]string{"1.1.1.1"}, updates, false, false)

	// A healthy reply: RTT well under the warn threshold.
	mm, _ := m.Update(statsMsg{TargetID: "1.1.1.1", Sent: 1, Recv: 1, RTT: 5 * time.Millisecond})
	out := mm.(Model)
	if got := rttLevel(out.stats["1.1.1.1"]); got != levelGood {
		t.Fatalf("expected levelGood after healthy reply, got %d", got)
	}
	if got := formatRTT(out.stats["1.1.1.1"]); got != "5ms" {
		t.Fatalf("expected RTT=5ms after healthy reply, got %q", got)
	}

	// A timed-out round: Sent increments, Recv does not, TimedOut=true.
	// RTT must still read "5ms" (stale, not blanked) but classify as
	// critical.
	mm, _ = out.Update(statsMsg{TargetID: "1.1.1.1", Sent: 2, Recv: 1, TimedOut: true})
	out = mm.(Model)
	got := out.stats["1.1.1.1"]
	if !got.TimedOut {
		t.Fatalf("expected TimedOut=true to be stored, got %+v", got)
	}
	if formatted := formatRTT(got); formatted != "5ms" {
		t.Errorf("expected stale RTT=5ms still shown during timeout, got %q", formatted)
	}
	if level := rttLevel(got); level != levelCrit {
		t.Errorf("expected levelCrit during timeout, got %d", level)
	}

	// A subsequent real reply clears TimedOut and re-evaluates level
	// on the fresh RTT.
	mm, _ = out.Update(statsMsg{TargetID: "1.1.1.1", Sent: 3, Recv: 2, RTT: 6 * time.Millisecond})
	out = mm.(Model)
	got = out.stats["1.1.1.1"]
	if got.TimedOut {
		t.Errorf("expected TimedOut=false after a real reply, got true")
	}
	if level := rttLevel(got); level != levelGood {
		t.Errorf("expected levelGood after recovering from timeout, got %d", level)
	}
}

func TestViewWhenAllTargetsDropped(t *testing.T) {
	updates := make(chan pinger.StatsUpdate, 4)
	m := newTestModel([]string{"1.1.1.1"}, updates, false, false)

	mm, _ := m.Update(statsMsg{TargetID: "1.1.1.1", Dropped: true})
	view := mm.(Model).View()
	if !strings.Contains(view, "paste an ip to start pinging") {
		t.Errorf("view should show empty-state message, got:\n%s", view)
	}
}

func TestKeepDroppedRetainsRow(t *testing.T) {
	updates := make(chan pinger.StatsUpdate, 4)
	m := newTestModel([]string{"1.1.1.1", "8.8.8.8"}, updates, true, false)

	mm, _ := m.Update(statsMsg{TargetID: "8.8.8.8", Sent: 5, Recv: 0, Dropped: true})
	out := mm.(Model)

	if len(out.order) != 2 {
		t.Errorf("keep-dropped should preserve order, got %v", out.order)
	}
	got, ok := out.stats["8.8.8.8"]
	if !ok {
		t.Fatalf("stats for dropped target should be retained")
	}
	if got.Sent != 5 || got.Recv != 0 {
		t.Errorf("final stats should be (5,0), got (%d,%d)", got.Sent, got.Recv)
	}
	view := out.View()
	if !strings.Contains(view, "8.8.8.8") || !strings.Contains(view, "100.0%") || !strings.Contains(view, "5/5") {
		t.Errorf("view should show row with 100%% loss and 5/5, got:\n%s", view)
	}
}

func TestFilterUpdateOnSlashKey(t *testing.T) {
	updates := make(chan pinger.StatsUpdate, 4)
	m := newTestModel([]string{"1.1.1.1", "8.8.8.8"}, updates, false, false)

	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	out := mm.(Model)
	if !out.filterMode {
		t.Fatalf("expected filterMode=true after '/'")
	}

	mm, _ = out.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'8'}})
	out = mm.(Model)
	mm, _ = out.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'.'}})
	out = mm.(Model)
	if out.filter != "8." {
		t.Errorf("expected filter=%q, got %q", "8.", out.filter)
	}

	mm, _ = out.Update(tea.KeyMsg{Type: tea.KeyEsc})
	out = mm.(Model)
	if out.filterMode {
		t.Errorf("expected filterMode=false after esc")
	}
	if out.filter != "" {
		t.Errorf("expected filter cleared after esc, got %q", out.filter)
	}
}

func TestRTTLevel(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    pinger.StatsUpdate
		want level
	}{
		{"no data", pinger.StatsUpdate{}, levelNeutral},
		{"err only", pinger.StatsUpdate{LastErr: errors.New("boom")}, levelCrit},
		{"good", pinger.StatsUpdate{Recv: 1, RTT: 30 * time.Millisecond}, levelGood},
		{"warn at boundary", pinger.StatsUpdate{Recv: 1, RTT: 50 * time.Millisecond}, levelWarn},
		{"warn", pinger.StatsUpdate{Recv: 1, RTT: 100 * time.Millisecond}, levelWarn},
		{"crit at boundary", pinger.StatsUpdate{Recv: 1, RTT: 200 * time.Millisecond}, levelCrit},
		{"crit", pinger.StatsUpdate{Recv: 1, RTT: 500 * time.Millisecond}, levelCrit},
		{"timed out overrides good RTT", pinger.StatsUpdate{Recv: 1, RTT: 5 * time.Millisecond, TimedOut: true}, levelCrit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rttLevel(tc.s); got != tc.want {
				t.Errorf("rttLevel = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestJitterLevel(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    pinger.StatsUpdate
		want level
	}{
		{"no data", pinger.StatsUpdate{}, levelNeutral},
		{"good", pinger.StatsUpdate{Recv: 2, Jitter: 1 * time.Millisecond}, levelGood},
		{"warn", pinger.StatsUpdate{Recv: 2, Jitter: 10 * time.Millisecond}, levelWarn},
		{"crit", pinger.StatsUpdate{Recv: 2, Jitter: 50 * time.Millisecond}, levelCrit},
		{"timed out overrides good jitter", pinger.StatsUpdate{Recv: 2, Jitter: 1 * time.Millisecond, TimedOut: true}, levelCrit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := jitterLevel(tc.s); got != tc.want {
				t.Errorf("jitterLevel = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestLossLevel(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    pinger.StatsUpdate
		want level
	}{
		{"no sends", pinger.StatsUpdate{}, levelNeutral},
		{"zero loss", pinger.StatsUpdate{Sent: 100, Recv: 100}, levelGood},
		{"warn", pinger.StatsUpdate{Sent: 100, Recv: 99}, levelWarn},
		{"crit at boundary", pinger.StatsUpdate{Sent: 100, Recv: 95}, levelCrit},
		{"all lost", pinger.StatsUpdate{Sent: 5, Recv: 0}, levelCrit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := lossLevel(tc.s); got != tc.want {
				t.Errorf("lossLevel = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestStylerDisabled(t *testing.T) {
	st := newStyler(false)
	for _, l := range []level{levelNeutral, levelGood, levelWarn, levelCrit} {
		if got := st.render("hello", l); got != "hello" {
			t.Errorf("disabled styler should be a no-op, got %q for level %d", got, l)
		}
	}
}

func TestStylerEnabledAddsANSI(t *testing.T) {
	// lipgloss strips colors when stdout isn't a TTY (which it isn't
	// under `go test`). Force ANSI so the renderer actually emits codes.
	old := lipgloss.DefaultRenderer().ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI)
	defer lipgloss.SetColorProfile(old)

	st := newStyler(true)
	if got := st.render("hello", levelNeutral); got != "hello" {
		t.Errorf("neutral should bypass coloring even when enabled, got %q", got)
	}
	for _, l := range []level{levelGood, levelWarn, levelCrit} {
		got := st.render("hello", l)
		if !strings.Contains(got, "\x1b[") {
			t.Errorf("level %d should add ANSI escape, got %q", l, got)
		}
		if !strings.Contains(got, "hello") {
			t.Errorf("level %d should preserve original text, got %q", l, got)
		}
	}
}

func TestSortIDsByRTT(t *testing.T) {
	updates := make(chan pinger.StatsUpdate)
	m := newTestModel([]string{"slow", "fast", "mid", "nodata"}, updates, false, false)
	m.stats["fast"] = pinger.StatsUpdate{RTT: 5 * time.Millisecond, Sent: 1, Recv: 1}
	m.stats["mid"] = pinger.StatsUpdate{RTT: 50 * time.Millisecond, Sent: 1, Recv: 1}
	m.stats["slow"] = pinger.StatsUpdate{RTT: 500 * time.Millisecond, Sent: 1, Recv: 1}

	m.sortCol = 1 // RTT
	m.sortDesc = true
	got := m.visibleIDs()
	want := []string{"slow", "mid", "fast", "nodata"}
	if !equalSlice(got, want) {
		t.Errorf("rtt desc: got %v, want %v", got, want)
	}

	m.sortDesc = false
	got = m.visibleIDs()
	want = []string{"fast", "mid", "slow", "nodata"}
	if !equalSlice(got, want) {
		t.Errorf("rtt asc: got %v, want %v", got, want)
	}
}

func TestSortIDsByLoss(t *testing.T) {
	updates := make(chan pinger.StatsUpdate)
	m := newTestModel([]string{"halflost", "clean", "alllost", "nodata"}, updates, false, false)
	m.stats["clean"] = pinger.StatsUpdate{Sent: 10, Recv: 10}
	m.stats["halflost"] = pinger.StatsUpdate{Sent: 10, Recv: 5}
	m.stats["alllost"] = pinger.StatsUpdate{Sent: 10, Recv: 0}

	m.sortCol = 6 // LOSS%
	m.sortDesc = true
	got := m.visibleIDs()
	want := []string{"alllost", "halflost", "clean", "nodata"}
	if !equalSlice(got, want) {
		t.Errorf("loss desc: got %v, want %v", got, want)
	}

	m.sortDesc = false
	got = m.visibleIDs()
	want = []string{"clean", "halflost", "alllost", "nodata"}
	if !equalSlice(got, want) {
		t.Errorf("loss asc: got %v, want %v", got, want)
	}
}

func TestSortIDsByTarget(t *testing.T) {
	updates := make(chan pinger.StatsUpdate)
	// Insertion order intentionally not alphabetical so we can tell
	// TARGET-sort from passthrough.
	m := newTestModel([]string{"charlie", "alpha", "bravo"}, updates, false, false)

	m.sortCol = 0 // TARGET
	m.sortDesc = false
	got := m.visibleIDs()
	want := []string{"alpha", "bravo", "charlie"}
	if !equalSlice(got, want) {
		t.Errorf("target asc: got %v, want %v", got, want)
	}

	m.sortDesc = true
	got = m.visibleIDs()
	want = []string{"charlie", "bravo", "alpha"}
	if !equalSlice(got, want) {
		t.Errorf("target desc: got %v, want %v", got, want)
	}
}

func TestSortIDsByJitter(t *testing.T) {
	updates := make(chan pinger.StatsUpdate)
	m := newTestModel([]string{"steady", "spiky", "mid"}, updates, false, false)
	m.stats["steady"] = pinger.StatsUpdate{Jitter: 1 * time.Millisecond, Sent: 1, Recv: 1}
	m.stats["mid"] = pinger.StatsUpdate{Jitter: 10 * time.Millisecond, Sent: 1, Recv: 1}
	m.stats["spiky"] = pinger.StatsUpdate{Jitter: 100 * time.Millisecond, Sent: 1, Recv: 1}

	m.sortCol = 5 // JITTER
	m.sortDesc = true
	got := m.visibleIDs()
	want := []string{"spiky", "mid", "steady"}
	if !equalSlice(got, want) {
		t.Errorf("jitter desc: got %v, want %v", got, want)
	}
}

func TestSortIDsNoneIsPassthrough(t *testing.T) {
	updates := make(chan pinger.StatsUpdate)
	m := newTestModel([]string{"b", "a", "c"}, updates, false, false)
	m.stats["a"] = pinger.StatsUpdate{RTT: 1 * time.Millisecond, Sent: 1, Recv: 1}
	m.stats["b"] = pinger.StatsUpdate{RTT: 99 * time.Millisecond, Sent: 1, Recv: 1}
	m.stats["c"] = pinger.StatsUpdate{RTT: 50 * time.Millisecond, Sent: 1, Recv: 1}

	got := m.visibleIDs()
	want := []string{"b", "a", "c"}
	if !equalSlice(got, want) {
		t.Errorf("sortCol=-1 should be passthrough: got %v, want %v", got, want)
	}
}

func TestSortCycleVisitsAllSortableColumns(t *testing.T) {
	updates := make(chan pinger.StatsUpdate)
	m := newTestModel([]string{"a"}, updates, false, false)
	// termWidth=0 means visibleColumns returns all columns, so the cycle
	// should walk every sortable column index then wrap to -1.
	want := []int{0, 1, 2, 3, 4, 5, 6, -1, 0}
	for i, w := range want {
		mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
		m = mm.(Model)
		if m.sortCol != w {
			t.Errorf("press %d: sortCol=%d, want %d", i+1, m.sortCol, w)
		}
	}
}

func TestSortCycleBackwardsThroughAllColumns(t *testing.T) {
	updates := make(chan pinger.StatsUpdate)
	m := newTestModel([]string{"a"}, updates, false, false)
	// From -1, S should jump to the last sortable column and walk back.
	want := []int{6, 5, 4, 3, 2, 1, 0, -1, 6}
	for i, w := range want {
		mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'S'}})
		m = mm.(Model)
		if m.sortCol != w {
			t.Errorf("press %d: sortCol=%d, want %d", i+1, m.sortCol, w)
		}
	}
}

func TestSortCycleSkipsHiddenColumns(t *testing.T) {
	updates := make(chan pinger.StatsUpdate)
	m := newTestModel([]string{"a"}, updates, false, false)
	// At termWidth=70 the responsive layout hides MIN/AVG/MAX (tier 3)
	// AND SENT/LOST (tier 2). Remaining sortable visible columns are
	// TARGET(0), RTT(1), JITTER(5), LOSS%(6). SPARK is visible but not
	// sortable and should be skipped.
	m.termWidth = 70
	want := []int{0, 1, 5, 6, -1, 0}
	for i, w := range want {
		mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
		m = mm.(Model)
		if m.sortCol != w {
			t.Errorf("press %d: sortCol=%d, want %d", i+1, m.sortCol, w)
		}
	}
}

func TestSortRevertsOnResizeHidingColumn(t *testing.T) {
	updates := make(chan pinger.StatsUpdate)
	m := newTestModel([]string{"a"}, updates, false, false)
	// Sort on MIN (col 2, tier 3), then resize narrow enough to hide
	// tier-3 columns. Sort should clear.
	m.sortCol = 2
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 70, Height: 24})
	m = mm.(Model)
	if m.sortCol != -1 {
		t.Errorf("hiding active sort column should revert sortCol to -1, got %d", m.sortCol)
	}
}

func TestSortSurvivesResizeKeepingColumn(t *testing.T) {
	updates := make(chan pinger.StatsUpdate)
	m := newTestModel([]string{"a"}, updates, false, false)
	// Sort on RTT (col 1, tier 0 — always visible). Even a narrow resize
	// should keep the sort active.
	m.sortCol = 1
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 24})
	m = mm.(Model)
	if m.sortCol != 1 {
		t.Errorf("tier-0 sort should survive resize, got sortCol=%d", m.sortCol)
	}
}

func TestSortDirToggle(t *testing.T) {
	updates := make(chan pinger.StatsUpdate)
	m := newTestModel([]string{"a"}, updates, false, false)

	// Unsorted: r is a no-op (sortDesc stays at the New() default).
	startDesc := m.sortDesc
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	m = mm.(Model)
	if m.sortDesc != startDesc {
		t.Errorf("r while unsorted should be no-op: sortDesc=%v, want %v", m.sortDesc, startDesc)
	}

	// Engage a sort, then r flips direction each press.
	m.sortCol = 1
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	m = mm.(Model)
	if m.sortDesc == startDesc {
		t.Errorf("r while sorted should flip sortDesc, got %v", m.sortDesc)
	}
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	m = mm.(Model)
	if m.sortDesc != startDesc {
		t.Errorf("second r should flip back, got %v", m.sortDesc)
	}
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestVisibleColumns(t *testing.T) {
	// Headers in tier order so the assertions read naturally.
	const (
		full    = "TARGET,RTT,MIN,AVG,MAX,JITTER,LOSS%,SENT/LOST,TTL,SPARK"
		noMMM   = "TARGET,RTT,JITTER,LOSS%,SENT/LOST,TTL,SPARK"
		noSent  = "TARGET,RTT,JITTER,LOSS%,SPARK"
		noSpark = "TARGET,RTT,JITTER,LOSS%"
	)
	for _, tc := range []struct {
		name      string
		termWidth int
		want      string
	}{
		{"unset: render all", 0, full},
		{"exactly fits all", 126, full},
		{"one shy of all: drop MIN/AVG/MAX", 125, noMMM},
		{"fits without MIN/AVG/MAX", 96, noMMM},
		{"one shy: drop SENT/LOST and TTL too", 95, noSent},
		{"fits without SENT/LOST and TTL", 78, noSent},
		{"one shy: drop SPARK too", 77, noSpark},
		{"fits at minimum", 56, noSpark},
		{"narrower than minimum: stay at 4", 30, noSpark},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idx := visibleColumns(tc.termWidth)
			got := make([]string, len(idx))
			for i, ci := range idx {
				got[i] = columns[ci].header
			}
			if joined := strings.Join(got, ","); joined != tc.want {
				t.Errorf("termWidth=%d:\n got=%q\nwant=%q", tc.termWidth, joined, tc.want)
			}
		})
	}
}

// --- Clipboard paste (add/remove toggle) tests ---

func TestReadPasteAddsNewTarget(t *testing.T) {
	updates := make(chan pinger.StatsUpdate)
	cmds := make(chan TargetCmd, 4)
	m := New([]string{"1.1.1.1"}, updates, false, false, cmds, 256)
	m.readClip = func() (string, error) { return "8.8.8.8", nil }

	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlV})
	m = mm.(Model)
	if cmd == nil {
		t.Fatal("ctrl+v should produce a Cmd")
	}
	msg := cmd()
	mm, _ = m.Update(msg)
	out := mm.(Model)

	if !equalSlice(out.order, []string{"1.1.1.1", "8.8.8.8"}) {
		t.Errorf("expected 8.8.8.8 appended, got %v", out.order)
	}
	select {
	case c := <-cmds:
		if c.Action != ActionAdd || c.ID != "8.8.8.8" {
			t.Errorf("expected ActionAdd for 8.8.8.8, got %+v", c)
		}
	default:
		t.Fatal("expected a TargetCmd on cmds channel")
	}
	if !strings.Contains(out.pasteMsg, "added") {
		t.Errorf("expected pasteMsg to mention 'added', got %q", out.pasteMsg)
	}
}

func TestReadPasteTogglesRemoveExistingTarget(t *testing.T) {
	updates := make(chan pinger.StatsUpdate)
	cmds := make(chan TargetCmd, 4)
	m := New([]string{"1.1.1.1", "8.8.8.8"}, updates, false, false, cmds, 256)
	m.stats["8.8.8.8"] = pinger.StatsUpdate{Sent: 3, Recv: 3}
	m.readClip = func() (string, error) { return "8.8.8.8", nil }

	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlV})
	m = mm.(Model)
	msg := cmd()
	mm, _ = m.Update(msg)
	out := mm.(Model)

	if !equalSlice(out.order, []string{"1.1.1.1"}) {
		t.Errorf("expected 8.8.8.8 removed, got %v", out.order)
	}
	if _, ok := out.stats["8.8.8.8"]; ok {
		t.Errorf("expected stats for 8.8.8.8 to be cleared")
	}
	select {
	case c := <-cmds:
		if c.Action != ActionRemove || c.ID != "8.8.8.8" {
			t.Errorf("expected ActionRemove for 8.8.8.8, got %+v", c)
		}
	default:
		t.Fatal("expected a TargetCmd on cmds channel")
	}
	if !strings.Contains(out.pasteMsg, "removed") {
		t.Errorf("expected pasteMsg to mention 'removed', got %q", out.pasteMsg)
	}
}

func TestReadPasteMultiLineMixedAddRemove(t *testing.T) {
	updates := make(chan pinger.StatsUpdate)
	cmds := make(chan TargetCmd, 8)
	// 8.8.8.8 already tracked (will be removed); 9.9.9.9 and
	// example.com are new (will be added).
	m := New([]string{"1.1.1.1", "8.8.8.8"}, updates, false, false, cmds, 256)
	m.readClip = func() (string, error) {
		return "8.8.8.8\n9.9.9.9\nexample.com\n", nil
	}

	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlV})
	m = mm.(Model)
	msg := cmd()
	mm, _ = m.Update(msg)
	out := mm.(Model)

	want := []string{"1.1.1.1", "9.9.9.9", "example.com"}
	if !equalSlice(out.order, want) {
		t.Errorf("got order %v, want %v", out.order, want)
	}
	if !strings.Contains(out.pasteMsg, "added") || !strings.Contains(out.pasteMsg, "removed") {
		t.Errorf("expected pasteMsg to mention both added and removed, got %q", out.pasteMsg)
	}
}

func TestReadPasteIgnoresBlankLinesAndBadEntries(t *testing.T) {
	updates := make(chan pinger.StatsUpdate)
	cmds := make(chan TargetCmd, 4)
	m := New([]string{}, updates, false, false, cmds, 256)
	m.readClip = func() (string, error) {
		return "\n  \n1.1.1.1\n!!!not-a-target!!!\n\n", nil
	}

	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlV})
	m = mm.(Model)
	msg := cmd()
	mm, _ = m.Update(msg)
	out := mm.(Model)

	if !equalSlice(out.order, []string{"1.1.1.1"}) {
		t.Errorf("expected only 1.1.1.1 added, got %v", out.order)
	}
}

func TestReadPasteEmptyClipboardShowsError(t *testing.T) {
	updates := make(chan pinger.StatsUpdate)
	m := New([]string{}, updates, false, false, nil, 256)
	m.readClip = func() (string, error) { return "   \n  \n", nil }

	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlV})
	m = mm.(Model)
	msg := cmd()
	mm, _ = m.Update(msg)
	out := mm.(Model)

	if !strings.Contains(out.pasteMsg, "paste:") {
		t.Errorf("expected an error paste message, got %q", out.pasteMsg)
	}
	if len(out.order) != 0 {
		t.Errorf("expected no targets added on empty clipboard, got %v", out.order)
	}
}

func TestReadPasteClipboardReadError(t *testing.T) {
	updates := make(chan pinger.StatsUpdate)
	m := New([]string{}, updates, false, false, nil, 256)
	m.readClip = func() (string, error) { return "", fmt.Errorf("no clipboard tool found") }

	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlV})
	m = mm.(Model)
	msg := cmd()
	mm, _ = m.Update(msg)
	out := mm.(Model)

	if !strings.Contains(out.pasteMsg, "clipboard") {
		t.Errorf("expected clipboard error surfaced in pasteMsg, got %q", out.pasteMsg)
	}
}

func TestReadPasteRespectsMaxHosts(t *testing.T) {
	updates := make(chan pinger.StatsUpdate)
	m := New([]string{}, updates, false, false, nil, 2)
	m.readClip = func() (string, error) { return "10.0.0.0/29", nil } // expands to 6 hosts

	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlV})
	m = mm.(Model)
	msg := cmd()
	mm, _ = m.Update(msg)
	out := mm.(Model)

	if len(out.order) != 0 {
		t.Errorf("expected no targets added when paste exceeds max-hosts, got %v", out.order)
	}
	if !strings.Contains(out.pasteMsg, "paste:") {
		t.Errorf("expected an error paste message, got %q", out.pasteMsg)
	}
}

func TestClearPasteStatusMsgClearsBanner(t *testing.T) {
	updates := make(chan pinger.StatsUpdate)
	m := New([]string{}, updates, false, false, nil, 256)
	m.pasteMsg = "paste: +1 target(s) added"

	mm, _ := m.Update(clearPasteStatusMsg{})
	out := mm.(Model)
	if out.pasteMsg != "" {
		t.Errorf("expected pasteMsg cleared, got %q", out.pasteMsg)
	}
}

func TestPasteNilCmdsChannelDoesNotPanic(t *testing.T) {
	updates := make(chan pinger.StatsUpdate)
	m := New([]string{}, updates, false, false, nil, 256)
	m.readClip = func() (string, error) { return "1.1.1.1", nil }

	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlV})
	m = mm.(Model)
	msg := cmd()
	// Should not panic even though cmds is nil.
	mm, _ = m.Update(msg)
	out := mm.(Model)
	if !equalSlice(out.order, []string{"1.1.1.1"}) {
		t.Errorf("expected 1.1.1.1 added even with nil cmds channel, got %v", out.order)
	}
}

// --- Clear (C) and reset-stats (R) tests ---

func TestClearRemovesAllTargetsAndSendsStopCommands(t *testing.T) {
	updates := make(chan pinger.StatsUpdate)
	cmds := make(chan TargetCmd, 8)
	m := New([]string{"1.1.1.1", "8.8.8.8"}, updates, false, false, cmds, 256)
	m.stats["1.1.1.1"] = pinger.StatsUpdate{Sent: 5, Recv: 5}
	m.history["1.1.1.1"] = []time.Duration{time.Millisecond}

	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'C'}})
	out := mm.(Model)

	if len(out.order) != 0 {
		t.Errorf("expected order empty after clear, got %v", out.order)
	}
	if len(out.stats) != 0 {
		t.Errorf("expected stats empty after clear, got %v", out.stats)
	}
	if len(out.history) != 0 {
		t.Errorf("expected history empty after clear, got %v", out.history)
	}

	gotIDs := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case c := <-cmds:
			if c.Action != ActionRemove {
				t.Errorf("expected ActionRemove, got %+v", c)
			}
			gotIDs[c.ID] = true
		default:
			t.Fatal("expected a TargetCmd on cmds channel")
		}
	}
	if !gotIDs["1.1.1.1"] || !gotIDs["8.8.8.8"] {
		t.Errorf("expected remove commands for both targets, got %v", gotIDs)
	}
}

func TestClearWithNilCmdsChannelDoesNotPanic(t *testing.T) {
	updates := make(chan pinger.StatsUpdate)
	m := New([]string{"1.1.1.1"}, updates, false, false, nil, 256)

	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'C'}})
	out := mm.(Model)
	if len(out.order) != 0 {
		t.Errorf("expected order empty after clear, got %v", out.order)
	}
}

func TestClearOnEmptyTableIsNoOp(t *testing.T) {
	updates := make(chan pinger.StatsUpdate)
	cmds := make(chan TargetCmd, 8)
	m := New([]string{}, updates, false, false, cmds, 256)

	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'C'}})
	out := mm.(Model)
	if len(out.order) != 0 {
		t.Errorf("expected order still empty, got %v", out.order)
	}
	select {
	case c := <-cmds:
		t.Errorf("expected no TargetCmd when clearing an empty table, got %+v", c)
	default:
	}
}

func TestResetStatsKeepsTargetsClearsStatsAndHistory(t *testing.T) {
	updates := make(chan pinger.StatsUpdate)
	cmds := make(chan TargetCmd, 8)
	m := New([]string{"1.1.1.1", "8.8.8.8"}, updates, false, false, cmds, 256)
	m.stats["1.1.1.1"] = pinger.StatsUpdate{Sent: 10, Recv: 8, RTT: 5 * time.Millisecond}
	m.stats["8.8.8.8"] = pinger.StatsUpdate{Sent: 3, Recv: 3}
	m.history["1.1.1.1"] = []time.Duration{time.Millisecond, 2 * time.Millisecond}

	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'R'}})
	out := mm.(Model)

	if !equalSlice(out.order, []string{"1.1.1.1", "8.8.8.8"}) {
		t.Errorf("expected targets to remain after reset, got %v", out.order)
	}
	if len(out.stats) != 0 {
		t.Errorf("expected stats cleared after reset, got %v", out.stats)
	}
	if len(out.history) != 0 {
		t.Errorf("expected history cleared after reset, got %v", out.history)
	}
	select {
	case c := <-cmds:
		t.Errorf("reset should not send any TargetCmd (pingers keep running), got %+v", c)
	default:
	}
}

func TestLowercaseRStillTogglesSortDirectionNotReset(t *testing.T) {
	// Regression guard: adding uppercase "R" for reset-stats must not
	// disturb the existing lowercase "r" sort-direction-toggle binding.
	updates := make(chan pinger.StatsUpdate)
	m := New([]string{"a"}, updates, false, false, nil, 256)
	m.stats["a"] = pinger.StatsUpdate{Sent: 5, Recv: 5}
	m.sortCol = 1

	startDesc := m.sortDesc
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	out := mm.(Model)
	if out.sortDesc == startDesc {
		t.Errorf("lowercase r should still flip sortDesc, got %v", out.sortDesc)
	}
	// Stats must be untouched by lowercase r.
	if _, ok := out.stats["a"]; !ok {
		t.Errorf("lowercase r should not clear stats")
	}
}

// --- Sparkline ASCII/Unicode toggle ("t") tests ---

func TestToggleSparkAsciiKey(t *testing.T) {
	updates := make(chan pinger.StatsUpdate)
	m := New([]string{"a"}, updates, false, false, nil, 256)
	if m.sparkAscii {
		t.Fatal("sparkAscii should default to false (Unicode bars)")
	}

	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	out := mm.(Model)
	if !out.sparkAscii {
		t.Error("expected sparkAscii=true after first 't' press")
	}

	mm, _ = out.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	out = mm.(Model)
	if out.sparkAscii {
		t.Error("expected sparkAscii=false after second 't' press")
	}
}

func TestFormatSparkUsesASCIISetWhenToggled(t *testing.T) {
	h := []time.Duration{1 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond}
	got := formatSpark(h, sparkWidth, true)
	runes := []rune(got)
	last3 := runes[len(runes)-3:]
	if last3[0] != sparkBarsASCII[0] {
		t.Errorf("min sample should map to ASCII %c, got %c", sparkBarsASCII[0], last3[0])
	}
	if last3[2] != sparkBarsASCII[len(sparkBarsASCII)-1] {
		t.Errorf("max sample should map to ASCII %c, got %c", sparkBarsASCII[len(sparkBarsASCII)-1], last3[2])
	}
	for _, r := range got {
		if r > 127 {
			t.Errorf("ascii=true should never emit non-ASCII runes, got %q in %q", r, got)
		}
	}
}

func TestFormatSparkUsesUnicodeSetByDefault(t *testing.T) {
	h := []time.Duration{1 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond}
	got := formatSpark(h, sparkWidth, false)
	runes := []rune(got)
	last3 := runes[len(runes)-3:]
	if last3[2] != sparkBarsUnicode[len(sparkBarsUnicode)-1] {
		t.Errorf("max sample should map to Unicode %c, got %c", sparkBarsUnicode[len(sparkBarsUnicode)-1], last3[2])
	}
}
