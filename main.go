package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/guerrieroriccardo/pingtop/internal/pinger"
	"github.com/guerrieroriccardo/pingtop/internal/target"
	"github.com/guerrieroriccardo/pingtop/internal/ui"
	"github.com/guerrieroriccardo/pingtop/internal/update"
)

// version is set at build time via -ldflags "-X main.version=...",
// as goreleaser's config here already does. Left as "dev" for
// unadorned `go build` — the ui package treats "dev" (and "") as
// "update checking disabled", since there's nothing meaningful to
// compare a version-less local build against.
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "pingtop:", err)
		os.Exit(1)
	}
}

func run() error {
	// Best-effort cleanup of a "<exe>.old" left behind by a previous
	// self-update (see internal/update.ReplaceAndRelaunch) — safe to
	// attempt on every startup regardless of whether one exists.
	update.CleanupOldExe()

	interval := flag.Duration("i", time.Second, "interval between pings")

	var maxHosts int
	flag.IntVar(&maxHosts, "max-hosts", 256, "hard cap on the number of expanded targets")
	flag.IntVar(&maxHosts, "m", 256, "alias for --max-hosts")

	var drop int
	flag.IntVar(&drop, "drop", 0, "drop a target after this many sends with no replies (0=disabled)")
	flag.IntVar(&drop, "d", 0, "alias for --drop")

	var keepDropped bool
	flag.BoolVar(&keepDropped, "keep-dropped", false, "keep dropped rows in the table (final stats stay visible)")
	flag.BoolVar(&keepDropped, "k", false, "alias for --keep-dropped")

	var noColor bool
	flag.BoolVar(&noColor, "no-color", false, "disable color output (NO_COLOR env var also honored)")

	size := flag.Int("size", 24, "ICMP payload size in bytes")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: pingtop [flags] [target...]\n\n")
		fmt.Fprintf(os.Stderr, "Targets may be IPs, hostnames, or CIDR ranges. All are optional —\n")
		fmt.Fprintf(os.Stderr, "run with none to start empty.\n\n")
		fmt.Fprintf(os.Stderr, "While running, press ctrl+v to paste a target (IP, subnet, or\n")
		fmt.Fprintf(os.Stderr, "hostname) from the clipboard and start pinging it. Pasting a\n")
		fmt.Fprintf(os.Stderr, "target that's already being pinged removes it instead.\n\nflags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	mode, err := pinger.DetectMode()
	if err != nil {
		return err
	}

	// flag.Args() may be empty — that's the "start empty, add targets
	// via ctrl+v paste" mode. target.Expand handles a nil/empty slice
	// fine (returns an empty, non-error result), so no special-casing
	// is needed here beyond skipping straight through.
	targets, err := target.Expand(flag.Args(), maxHosts)
	if err != nil {
		return err
	}

	ids := make([]string, len(targets))
	for i, t := range targets {
		ids[i] = t.ID
	}

	// Buffer for ~4 events per target keeps emit() non-blocking under
	// typical load (1 Hz ping rate, sub-second UI redraw cadence). The
	// buffer is sized for the *initial* target count; targets added
	// later via paste share the same channel and headroom, which is
	// fine since paste-added targets are the exception, not the norm.
	updates := make(chan pinger.StatsUpdate, len(targets)*4+64)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// cmds carries add/remove instructions from paste events in the UI
	// to the pinger manager below. Buffered generously: a multi-line
	// paste can emit several commands in one burst and the UI's Update
	// loop must not block waiting for the manager to drain them.
	cmds := make(chan ui.TargetCmd, 64)

	mgr := newPingerManager(ctx, mode, *interval, *size, drop, updates)
	mgr.addAll(targets)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		mgr.run(cmds)
	}()

	// Per the NO_COLOR spec (no-color.org), any non-empty value of the
	// env var disables color — including "0". Don't strconv-parse it.
	colorize := !noColor && os.Getenv("NO_COLOR") == ""

	prog := tea.NewProgram(ui.New(ids, updates, keepDropped, colorize, cmds, maxHosts, version), tea.WithAltScreen())
	finalModel, runErr := prog.Run()

	cancel()
	mgr.wait()
	wg.Wait()

	if runErr != nil {
		return fmt.Errorf("ui: %w", runErr)
	}

	// If the person pressed "U" and the download succeeded, the model
	// quit with a pending install path set rather than performing the
	// filesystem swap itself — doing it here, after tea.Program has
	// fully restored the terminal (exited the alt screen, restored
	// the cursor), avoids corrupting the screen mid-update. See
	// ui.Model.PendingInstall's doc.
	if m, ok := finalModel.(ui.Model); ok {
		if path := m.PendingInstall(); path != "" {
			if err := update.ReplaceAndRelaunch(path); err != nil {
				return fmt.Errorf("update: %w", err)
			}
			// The new process is already running independently; this
			// one has nothing left to do.
		}
	}
	return nil
}

// pingerManager owns the lifecycle of every running Pinger goroutine
// and reacts to add/remove/reset commands coming from the UI's
// clipboard paste handler and reset-stats command. It centralizes the
// per-target context.CancelFunc bookkeeping that a dynamic (as
// opposed to fixed-at-startup) set of targets requires.
type pingerManager struct {
	ctx      context.Context
	mode     pinger.Mode
	interval time.Duration
	size     int
	drop     int
	updates  chan<- pinger.StatsUpdate

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
	hosts   map[string]string    // id -> host, so ActionReset can restart without the UI re-sending Host
	done    map[string]chan struct{} // id -> channel closed when that id's goroutine returns
	wg      sync.WaitGroup
}

func newPingerManager(ctx context.Context, mode pinger.Mode, interval time.Duration, size, drop int, updates chan<- pinger.StatsUpdate) *pingerManager {
	return &pingerManager{
		ctx:      ctx,
		mode:     mode,
		interval: interval,
		size:     size,
		drop:     drop,
		updates:  updates,
		cancels:  make(map[string]context.CancelFunc),
		hosts:    make(map[string]string),
		done:     make(map[string]chan struct{}),
	}
}

// addAll starts one pinger per target. Used once at startup for the
// targets given on the command line.
func (m *pingerManager) addAll(targets []target.Target) {
	for _, t := range targets {
		m.start(t.ID, t.Host)
	}
}

// start launches a pinger for the given id/host pair, unless one is
// already running for that id (a defensive no-op — the UI's toggle
// logic should never call start on an id it thinks is already
// tracked, but a duplicate start here would leak a goroutine and a
// stray cancel func, so guard it anyway).
func (m *pingerManager) start(id, host string) {
	m.mu.Lock()
	if _, exists := m.cancels[id]; exists {
		m.mu.Unlock()
		return
	}
	pCtx, cancel := context.WithCancel(m.ctx)
	doneCh := make(chan struct{})
	m.cancels[id] = cancel
	m.hosts[id] = host
	m.done[id] = doneCh
	m.mu.Unlock()

	p := &pinger.Pinger{
		ID:       id,
		Host:     host,
		Mode:     m.mode,
		Interval: m.interval,
		Size:     m.size,
		Drop:     m.drop,
		Updates:  m.updates,
	}

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		_ = p.Run(pCtx)
		m.mu.Lock()
		delete(m.cancels, id)
		delete(m.hosts, id)
		delete(m.done, id)
		m.mu.Unlock()
		close(doneCh)
	}()
}

// stop cancels the pinger for id, if one is running, and returns a
// channel that's closed once that pinger's goroutine has actually
// returned — IcmpSendEcho (Windows) and pro-bing's Run (Unix) both
// block synchronously inside a single send/receive cycle, so
// cancelling the context doesn't mean the goroutine has stopped yet;
// it can take up to that cycle's timeout to unwind. Callers that need
// to know the pinger is really gone (see reset, which must not start
// a replacement while the old one might still be running) should wait
// on the returned channel. Callers that only want to signal
// cancellation and move on (e.g. ActionRemove) can ignore it. Returns
// nil if id wasn't running.
func (m *pingerManager) stop(id string) <-chan struct{} {
	m.mu.Lock()
	cancel, exists := m.cancels[id]
	doneCh := m.done[id]
	m.mu.Unlock()
	if !exists {
		return nil
	}
	cancel()
	return doneCh
}

// reset restarts id's pinger from scratch: stop the current goroutine,
// wait for it to actually finish (see stop's doc — cancellation isn't
// synchronous), and only then start a fresh one with the same host and
// zeroed counters. Runs the wait+restart in its own goroutine so a
// slow-to-unwind pinger (waiting out an in-flight echo's timeout)
// doesn't block pingerManager.run from processing other commands
// (other targets' resets, adds, removes) in the meantime.
func (m *pingerManager) reset(id string) {
	m.mu.Lock()
	host, exists := m.hosts[id]
	m.mu.Unlock()
	if !exists {
		return
	}
	doneCh := m.stop(id)
	go func() {
		if doneCh != nil {
			select {
			case <-doneCh:
			case <-m.ctx.Done():
				return // program shutting down; don't restart
			}
		}
		m.start(id, host)
	}()
}

// run consumes TargetCmds from cmds until it's closed or the manager's
// context is cancelled (program shutdown). Call this in its own
// goroutine; it blocks until one of those two things happens.
func (m *pingerManager) run(cmds <-chan ui.TargetCmd) {
	for {
		select {
		case <-m.ctx.Done():
			return
		case c, ok := <-cmds:
			if !ok {
				return
			}
			switch c.Action {
			case ui.ActionAdd:
				m.start(c.ID, c.Host)
			case ui.ActionRemove:
				m.stop(c.ID)
			case ui.ActionReset:
				m.reset(c.ID)
			}
		}
	}
}

// wait blocks until every pinger goroutine started by this manager has
// returned. Call after cancelling the manager's context so Run() calls
// actually unblock.
func (m *pingerManager) wait() {
	m.wg.Wait()
}
