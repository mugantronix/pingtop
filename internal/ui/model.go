// Package ui renders the pingtop dashboard with Bubble Tea.
package ui

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"

	"github.com/guerrieroriccardo/pingtop/internal/clipboard"
	"github.com/guerrieroriccardo/pingtop/internal/pinger"
	"github.com/guerrieroriccardo/pingtop/internal/target"
	"github.com/guerrieroriccardo/pingtop/internal/update"
)

// columnDef describes one table column. Order in the columns slice
// matches the cells emitted by buildRows. tier drives responsive
// hiding: tier 0 columns are always shown; higher-tier columns are
// dropped first when the terminal is too narrow to fit them all.
// sortable=true means the column participates in the `s` sort cycle.
type columnDef struct {
	header   string
	width    int
	tier     int
	sortable bool
}

// SENT/LOST and TTL are intentionally non-sortable: SENT/LOST's only
// meaningful scalar is loss%, which the LOSS% column already covers,
// and TTL is diagnostic metadata (route/hop info) rather than a
// health metric worth ranking by. SPARK has no scalar either.
var columns = []columnDef{
	{header: "TARGET", width: 28, tier: 0, sortable: true},
	{header: "RTT", width: 10, tier: 0, sortable: true},
	{header: "MIN", width: 10, tier: 3, sortable: true},
	{header: "AVG", width: 10, tier: 3, sortable: true},
	{header: "MAX", width: 10, tier: 3, sortable: true},
	{header: "JITTER", width: 10, tier: 0, sortable: true},
	{header: "LOSS%", width: 8, tier: 0, sortable: true},
	{header: "SENT/LOST", width: 12, tier: 2, sortable: false},
	{header: "TTL", width: 6, tier: 2, sortable: false},
	{header: "SPARK", width: sparkWidth + 2, tier: 1, sortable: false},
}

// statsMsg wraps a StatsUpdate so it can flow through the Bubble Tea
// message bus without exposing pinger types as a top-level tea.Msg.
type statsMsg pinger.StatsUpdate

// TargetAction tells main's pinger manager what to do with a target ID
// resulting from a clipboard paste.
type TargetAction int

const (
	// ActionAdd starts a new pinger for a target not currently tracked.
	ActionAdd TargetAction = iota
	// ActionRemove stops and drops a target that's already tracked -
	// this is the "paste again to remove" toggle behavior.
	ActionRemove
	// ActionReset restarts a target's pinger from scratch (fresh
	// Sent/Recv/RTT counters), keeping it under active ping. Used by
	// the "R" reset-stats command: the UI-side stats/history maps are
	// cleared locally, but the pinger goroutine itself accumulates its
	// own running counters independently (see run_windows.go/
	// run_unix.go), so clearing the UI's copy alone isn't enough - the
	// next StatsUpdate from an un-reset pinger would just repopulate
	// the old cumulative numbers. Restarting the goroutine is the
	// simplest way to zero that internal state too.
	ActionReset
)

// TargetCmd is one add/remove instruction produced by a clipboard
// paste, sent on Model's cmds channel for main to act on.
type TargetCmd struct {
	Action TargetAction
	ID     string
	Host   string // resolver-facing host; equals ID except for future non-1:1 cases
}

// pasteResultMsg carries the outcome of a clipboard read+parse back
// into the Bubble Tea update loop (tea.Cmd results must be tea.Msg).
type pasteResultMsg struct {
	targets []target.Target
	err     error
}

// pasteStatusDuration is how long the paste banner (success or error)
// stays visible before yielding back to the normal help/status line.
const pasteStatusDuration = 3 * time.Second

// clearPasteStatusMsg fires after pasteStatusDuration to clear the banner.
type clearPasteStatusMsg struct{}

// updateCheckInterval is how often Model re-checks GitHub for a newer
// release after the initial check on startup.
const updateCheckInterval = time.Hour

// updateCheckTimeout bounds a single check-for-update network call —
// this is a background courtesy check, not something the person is
// waiting on, so it should give up quietly well before it could ever
// feel like the program hung.
const updateCheckTimeout = 10 * time.Second

// updateDownloadTimeout bounds the download+verify Cmd triggered by
// pressing "U". Generous compared to updateCheckTimeout since it's
// fetching an actual multi-MB binary, but still bounded so a stalled
// connection doesn't leave the person stuck on the "downloading..."
// screen forever.
const updateDownloadTimeout = 2 * time.Minute

// updateCheckResultMsg carries the outcome of a background
// check-for-update Cmd back into the Bubble Tea update loop. A nil
// Release with a nil error means "checked successfully, nothing
// newer" — distinct from err != nil, which means the check itself
// failed (network error, GitHub API hiccup) and is silently ignored
// rather than shown to the person: a background version check is not
// something worth interrupting them about when it fails.
type updateCheckResultMsg struct {
	rel *update.Release
	err error
}

// scheduleNextCheckMsg fires updateCheckInterval after the previous
// check resolved, triggering the next one.
type scheduleNextCheckMsg struct{}

// updateDownloadedMsg carries the outcome of the download Cmd
// triggered by pressing "U".
type updateDownloadedMsg struct {
	path string
	err  error
}

// Model is the dashboard state. Construct it with New, then pass it to
// tea.NewProgram.
type Model struct {
	order       []string
	updates     <-chan pinger.StatsUpdate
	stats       map[string]pinger.StatsUpdate
	history     map[string][]time.Duration // per-target RTT ring buffer for the sparkline
	termWidth   int                        // last WindowSizeMsg width; 0 until first event (renders all columns)
	termHeight  int                        // last WindowSizeMsg height; 0 until first event
	offset      int                        // first row index shown when content overflows viewport
	filterMode  bool                       // true while user is typing into the filter
	filter      string                     // active filter; empty == no filter
	sortCol     int                        // -1 = no sort (insertion order); otherwise index into columns
	sortDesc    bool                       // direction when sortCol >= 0; default true (worst first)
	keepDropped bool                       // if true, dropped rows stay visible with final stats
	styler      styler                     // colors for RTT/JITTER/LOSS%; disabled styler is a no-op

	maxHosts   int              // cap passed through to target.Expand on every paste
	cmds       chan<- TargetCmd // add/remove instructions consumed by main's pinger manager
	pasteMsg   string           // transient status line shown after a paste (success or error)
	readClip   func() (string, error) // clipboard reader; swappable in tests

	// sparkAscii selects the SPARK column's character set: Unicode
	// eighth-block bars (▁▂▃▄▅▆▇█, the default) or a plain ASCII
	// density scale. Which one renders correctly depends on the
	// terminal's font/code page — Windows Console Host with certain
	// fonts shows the Unicode bars as boxes, while Windows Terminal
	// with Cascadia Mono renders them fine. There's no reliable way
	// for the program to detect this itself (font/glyph coverage
	// isn't queryable at this level — see the "t" key toggle below),
	// so the "t" key lets the person switch at runtime and keep
	// whichever looks right on their setup.
	sparkAscii bool

	// --- update check / self-update state ---
	version         string                                          // this build's own version (e.g. "1.0.0"); "" or "dev" disables the update check entirely
	checkUpdate     func(ctx context.Context) (*update.Release, error) // swappable in tests; nil disables checking
	downloadUpdate  func(ctx context.Context, rel *update.Release) (exePath string, err error) // swappable in tests; nil disables the "U" key
	updateAvailable bool
	latestRelease   *update.Release
	updating        bool   // true while a download is in flight, after pressing "U"
	pendingInstall  string // set once a downloaded update is ready to install; main.go checks this after tea.Program exits
}

// New builds the initial model. ids is the stable display order
// produced by target.Expand; updates is the shared channel the
// pingers publish on. If keepDropped is true, rows for targets that
// hit the drop threshold stay visible with their final (100% loss)
// stats instead of being removed. If colorize is true, the RTT,
// JITTER, and LOSS% cells are tinted by threshold.
//
// cmds is where add/remove instructions from clipboard pastes are
// sent; main's pinger manager reads it and starts/stops the
// corresponding goroutines. maxHosts bounds how many targets a single
// paste (which may contain a CIDR) can expand to, matching the
// command-line --max-hosts semantics.
//
// version is this build's own version string (e.g. "1.0.0"), shown at
// the right edge of the help line. An empty string or "dev" disables
// the update check entirely — there is nothing meaningful to compare
// a version-less dev build against.
func New(ids []string, updates <-chan pinger.StatsUpdate, keepDropped, colorize bool, cmds chan<- TargetCmd, maxHosts int, version string) Model {
	m := Model{
		order:       ids,
		updates:     updates,
		stats:       make(map[string]pinger.StatsUpdate, len(ids)),
		history:     make(map[string][]time.Duration, len(ids)),
		sortCol:     -1,
		sortDesc:    true,
		keepDropped: keepDropped,
		styler:      newStyler(colorize),
		maxHosts:    maxHosts,
		cmds:        cmds,
		readClip:    clipboard.Read,
		version:     version,
	}
	if version != "" && version != "dev" {
		m.checkUpdate = func(ctx context.Context) (*update.Release, error) {
			return update.FetchLatest(ctx)
		}
		m.downloadUpdate = func(ctx context.Context, rel *update.Release) (string, error) {
			return update.Download(ctx, rel)
		}
	}
	return m
}

// PendingInstall returns the local path of a downloaded update ready
// to be installed, or "" if none is pending. Only meaningful after
// tea.Program.Run() has returned (the model quit via the "U" flow,
// see Update's handling of updateDownloadedMsg) — main.go checks this
// on the final model to decide whether to call
// update.ReplaceAndRelaunch before exiting.
func (m Model) PendingInstall() string {
	return m.pendingInstall
}

// PendingInstallArgs returns the live target list (m.order) at the
// moment the update was triggered, formatted as command-line
// arguments for the relaunched process. This is what makes an update
// preserve targets added interactively after startup (via ctrl+v)
// rather than silently reverting to whatever the program was
// originally launched with — see ReplaceAndRelaunch's doc on why args
// is caller-supplied instead of defaulting to os.Args[1:].
func (m Model) PendingInstallArgs() []string {
	return append([]string(nil), m.order...)
}

func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.waitForUpdate()}
	if m.checkUpdate != nil {
		cmds = append(cmds, m.checkForUpdateCmd())
	}
	return tea.Batch(cmds...)
}

// waitForUpdate returns a Cmd that blocks on the updates channel and
// turns the next event into a statsMsg. When the channel is closed
// (main has stopped all pingers) it emits tea.Quit so the program
// exits cleanly.
func (m Model) waitForUpdate() tea.Cmd {
	ch := m.updates
	return func() tea.Msg {
		u, ok := <-ch
		if !ok {
			return tea.Quit()
		}
		return statsMsg(u)
	}
}

// readPaste returns a Cmd that reads the system clipboard and parses
// its contents via parseTargetsText. Kept as a fallback path for
// platforms/terminals where Ctrl+V does arrive as a distinct key
// event (see runeBuffer below for the Windows-console path, where it
// doesn't). Wired to tea.KeyCtrlV in Update.
func (m Model) readPaste() tea.Cmd {
	readClip := m.readClip
	maxHosts := m.maxHosts
	return func() tea.Msg {
		text, err := readClip()
		if err != nil {
			return pasteResultMsg{err: fmt.Errorf("clipboard: %w", err)}
		}
		return parseTargetsText(text, maxHosts)
	}
}

// parseTargetsText parses text as one target per line via
// target.Expand, tolerating blank lines and individually malformed
// entries - a paste with one bad line and nine good ones still adds
// the nine, rather than failing the whole paste. Only text with zero
// recognisable targets surfaces as an error. Used by readPaste
// (clipboard.Read → this).
func parseTargetsText(text string, maxHosts int) pasteResultMsg {
	lines := strings.Split(text, "\n")
	var args []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l != "" {
			args = append(args, l)
		}
	}
	if len(args) == 0 {
		return pasteResultMsg{err: fmt.Errorf("clipboard is empty")}
	}
	var ts []target.Target
	for _, a := range args {
		got, err := target.Expand([]string{a}, maxHosts)
		if err != nil {
			continue
		}
		ts = append(ts, got...)
	}
	if len(ts) == 0 {
		return pasteResultMsg{err: fmt.Errorf("no valid IP/subnet/hostname found")}
	}
	if len(ts) > maxHosts {
		return pasteResultMsg{err: fmt.Errorf("expands to %d targets; exceeds --max-hosts=%d", len(ts), maxHosts)}
	}
	return pasteResultMsg{targets: ts}
}

// clearPasteStatusAfter returns a Cmd that clears the paste banner
// after a delay, so it doesn't stick around forever.
func clearPasteStatusAfter(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return clearPasteStatusMsg{} })
}

// checkForUpdateCmd returns a Cmd that checks GitHub for a newer
// release. Nil if m.checkUpdate is nil (update checking disabled —
// see New's doc on the version parameter).
func (m Model) checkForUpdateCmd() tea.Cmd {
	checkFn := m.checkUpdate
	if checkFn == nil {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), updateCheckTimeout)
		defer cancel()
		rel, err := checkFn(ctx)
		return updateCheckResultMsg{rel: rel, err: err}
	}
}

// scheduleNextCheck returns a Cmd that fires scheduleNextCheckMsg
// after updateCheckInterval, so the update check repeats periodically
// without the person having to restart the program to notice a
// release that came out after they launched it.
func scheduleNextCheck() tea.Cmd {
	return tea.Tick(updateCheckInterval, func(time.Time) tea.Msg { return scheduleNextCheckMsg{} })
}

// downloadUpdateCmd returns a Cmd that downloads and verifies rel via
// m.downloadUpdate. Nil if m.downloadUpdate is nil or rel is nil —
// both should be impossible by the time this is called (gated by the
// "U" key handler in Update), but returning nil rather than a Cmd
// that immediately errors keeps the caller's logic simple either way.
func (m Model) downloadUpdateCmd() tea.Cmd {
	downloadFn := m.downloadUpdate
	rel := m.latestRelease
	if downloadFn == nil || rel == nil {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), updateDownloadTimeout)
		defer cancel()
		path, err := downloadFn(ctx, rel)
		return updateDownloadedMsg{path: path, err: err}
	}
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case statsMsg:
		if msg.Dropped {
			if m.keepDropped {
				// Persist the final snapshot (Sent=N, Recv=0) so the
				// row keeps showing 100% loss after the pinger stops.
				m.stats[msg.TargetID] = pinger.StatsUpdate(msg)
			} else {
				delete(m.stats, msg.TargetID)
				delete(m.history, msg.TargetID)
				m.order = removeID(m.order, msg.TargetID)
				clampOffset(&m)
			}
			return m, m.waitForUpdate()
		}
		u := pinger.StatsUpdate(msg)
		// A pre-send snapshot (emitted right before each echo goes
		// out, to keep Sent/loss% current between replies) always
		// carries RTT=0/Jitter=0 in the message itself, because the
		// reply hasn't arrived yet when it's built — that's "no NEW
		// data this tick", not "the reading is now zero". Overwriting
		// the display with that zero every cycle made the RTT/JITTER
		// cells flicker to the placeholder and back once per ping
		// interval. Detect a pre-send snapshot by Recv being unchanged
		// from the last stored reading (a real reply always increments
		// Recv), and carry the previous RTT/Jitter forward in that
		// case. rttLevel/formatRTT are Recv-gated (not RTT>0-gated) so
		// a genuine 0ms reply still displays correctly once it's
		// actually stored — this carry-forward only papers over the
		// synthetic zero in the pre-send message, it doesn't hide real
		// fast-LAN 0ms readings.
		if prev, ok := m.stats[msg.TargetID]; ok && u.Recv == prev.Recv && u.LastErr == nil {
			u.RTT = prev.RTT
			u.Jitter = prev.Jitter
		}
		m.stats[msg.TargetID] = u
		if msg.RTT > 0 {
			appendHistory(m.history, msg.TargetID, msg.RTT)
		}
		return m, m.waitForUpdate()

	case pasteResultMsg:
		if msg.err != nil {
			m.pasteMsg = "paste: " + msg.err.Error()
			return m, clearPasteStatusAfter(pasteStatusDuration)
		}
		var added, removed int
		alreadyTracked := make(map[string]bool, len(m.order))
		for _, id := range m.order {
			alreadyTracked[id] = true
		}
		for _, t := range msg.targets {
			if alreadyTracked[t.ID] {
				// Toggle: already under ping → remove it. The pinger
				// manager in main will cancel its goroutine; the row
				// itself disappears once main stops emitting for it
				// (or, for keep-dropped mode, is marked dropped) -
				// but we also proactively drop it from m.order here
				// so the UI reflects the removal immediately rather
				// than waiting on a race with the manager.
				if m.cmds != nil {
					m.cmds <- TargetCmd{Action: ActionRemove, ID: t.ID, Host: t.Host}
				}
				delete(m.stats, t.ID)
				delete(m.history, t.ID)
				m.order = removeID(m.order, t.ID)
				removed++
			} else {
				if m.cmds != nil {
					m.cmds <- TargetCmd{Action: ActionAdd, ID: t.ID, Host: t.Host}
				}
				m.order = append(m.order, t.ID)
				alreadyTracked[t.ID] = true
				added++
			}
		}
		clampOffset(&m)
		switch {
		case added > 0 && removed > 0:
			m.pasteMsg = fmt.Sprintf("paste: +%d added, -%d removed", added, removed)
		case added > 0:
			m.pasteMsg = fmt.Sprintf("paste: +%d target(s) added", added)
		case removed > 0:
			m.pasteMsg = fmt.Sprintf("paste: -%d target(s) removed", removed)
		}
		return m, clearPasteStatusAfter(pasteStatusDuration)

	case clearPasteStatusMsg:
		m.pasteMsg = ""
		return m, nil

	case updateCheckResultMsg:
		// A failed check (network hiccup, GitHub API error) is
		// silently ignored — see updateCheckResultMsg's doc. Either
		// way, schedule the next periodic check.
		if msg.err == nil && msg.rel != nil && update.IsNewer(m.version, msg.rel.Version) {
			m.updateAvailable = true
			m.latestRelease = msg.rel
		}
		return m, scheduleNextCheck()

	case scheduleNextCheckMsg:
		return m, m.checkForUpdateCmd()

	case updateDownloadedMsg:
		m.updating = false
		if msg.err != nil {
			m.pasteMsg = "update failed: " + msg.err.Error()
			return m, clearPasteStatusAfter(pasteStatusDuration)
		}
		// Hand off to main.go: quitting here (rather than calling
		// update.ReplaceAndRelaunch directly) lets Bubble Tea restore
		// the terminal (exit the alt screen, re-show the cursor)
		// before any filesystem surgery happens on the running exe.
		// See PendingInstall's doc.
		m.pendingInstall = msg.path
		return m, tea.Quit

	case tea.KeyMsg:
		debugLog("KeyMsg type=%v runes=%q str=%q alt=%v", msg.Type, msg.Runes, msg.String(), msg.Alt)
		if m.filterMode {
			switch msg.Type {
			case tea.KeyCtrlC:
				return m, tea.Quit
			case tea.KeyEsc:
				m.filterMode = false
				m.filter = ""
				clampOffset(&m)
			case tea.KeyEnter:
				m.filterMode = false
			case tea.KeyBackspace:
				if len(m.filter) > 0 {
					m.filter = m.filter[:len(m.filter)-1]
					clampOffset(&m)
				}
			case tea.KeySpace:
				m.filter += " "
				clampOffset(&m)
			case tea.KeyRunes:
				m.filter += string(msg.Runes)
				clampOffset(&m)
			}
			return m, nil
		}

		switch msg.Type {
		case tea.KeyCtrlV:
			return m, m.readPaste()
		}

		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "/":
			m.filterMode = true
			return m, nil
		case "esc":
			if m.filter != "" {
				m.filter = ""
				clampOffset(&m)
			}
			return m, nil
		case "up", "k":
			if m.offset > 0 {
				m.offset--
			}
			return m, nil
		case "down", "j":
			if m.offset < m.maxOffset() {
				m.offset++
			}
			return m, nil
		case "s":
			m.sortCol = nextSortCol(m.sortCol, visibleSortable(m.termWidth))
			clampOffset(&m)
			return m, nil
		case "S":
			m.sortCol = prevSortCol(m.sortCol, visibleSortable(m.termWidth))
			clampOffset(&m)
			return m, nil
		case "r":
			if m.sortCol >= 0 {
				m.sortDesc = !m.sortDesc
			}
			return m, nil
		case "R":
			// Reset stats: zero out every target's counters/history AND
			// tell main's pinger manager to restart each pinger's
			// goroutine (see ActionReset's doc for why the restart is
			// necessary, not just clearing the UI's copy). Distinct
			// from "C" (clear), which removes targets entirely instead
			// of restarting them.
			if m.cmds != nil {
				for _, id := range m.order {
					m.cmds <- TargetCmd{Action: ActionReset, ID: id}
				}
			}
			for id := range m.stats {
				delete(m.stats, id)
			}
			for id := range m.history {
				delete(m.history, id)
			}
			return m, nil
		case "C":
			// Clear: stop every active pinger and empty the table. Sent
			// as one ActionRemove per target, mirroring the paste-toggle
			// removal path so main's pinger manager stays the single
			// source of truth for which goroutines are running.
			if m.cmds != nil {
				for _, id := range m.order {
					m.cmds <- TargetCmd{Action: ActionRemove, ID: id}
				}
			}
			m.order = nil
			m.stats = make(map[string]pinger.StatsUpdate)
			m.history = make(map[string][]time.Duration)
			clampOffset(&m)
			return m, nil
		case "t":
			// Toggle the SPARK column between Unicode eighth-block bars
			// and a plain ASCII density scale — see sparkAscii's doc for
			// why this is a manual runtime choice rather than
			// auto-detected.
			m.sparkAscii = !m.sparkAscii
			return m, nil
		case "U":
			// Start downloading the update flagged by the background
			// check. Guarded on updateAvailable/latestRelease/
			// downloadUpdate so a stray "U" before a check completes
			// (or on a dev build with update checking disabled) is a
			// harmless no-op rather than a nil-pointer risk; `updating`
			// additionally prevents a second concurrent download if
			// the person mashes the key.
			if m.updateAvailable && m.latestRelease != nil && m.downloadUpdate != nil && !m.updating {
				m.updating = true
				return m, m.downloadUpdateCmd()
			}
			return m, nil
		}
		return m, nil

	case tea.WindowSizeMsg:
		m.termWidth = msg.Width
		m.termHeight = msg.Height
		// If the active sort column just got hidden by the resize, drop
		// the sort. Predictable state beats invisible sort.
		if m.sortCol >= 0 && !columnVisible(m.sortCol, m.termWidth) {
			m.sortCol = -1
		}
		clampOffset(&m)
		// Resize cycles drop/add columns, which makes individual rows
		// expand or contract horizontally. Bubble Tea's diff renderer
		// can leave stale fragments visible during rapid resizes, so
		// force a clean repaint on every WindowSizeMsg.
		return m, tea.ClearScreen
	}
	return m, nil
}

func (m Model) View() string {
	var text string
	switch {
	case m.filterMode:
		text = fmt.Sprintf("/%s█  [enter] apply  [esc] clear", m.filter)
	case m.updating:
		text = "downloading update..."
	case m.pasteMsg != "":
		text = m.pasteMsg + "  [ctrl+v] paste target"
	case len(m.order) == 0:
		text = "paste an ip to start pinging - [ctrl+v] paste  [q] quit"
	case m.filter != "":
		text = fmt.Sprintf("filter: %s  [/] edit  [esc] clear  [q] quit", m.filter)
	case m.sortCol >= 0:
		text = fmt.Sprintf("[q] quit  [/] filter  [↑/↓] scroll  [s/S] sort: %s %s  [r] reverse  [ctrl+v] paste  [C] clear  [R] reset  [t] spark",
			sortName(m.sortCol), sortArrow(m.sortDesc))
	default:
		text = "[q] quit  [/] filter  [↑/↓] scroll  [s/S] sort  [ctrl+v] paste  [C] clear  [R] reset  [t] spark"
	}

	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	green := lipgloss.NewStyle().Foreground(lipgloss.Color("10"))

	// Right-hand segment: version always shown (when known), with a
	// green "upgrade available" notice prepended when one is. Built
	// and measured in plain text first, then styled, so width math
	// below works against visible character counts rather than
	// ANSI-escaped strings.
	var rightPlain string
	if m.updateAvailable {
		rightPlain = "press [shift+u] to upgrade"
	}
	if m.version != "" {
		if rightPlain != "" {
			rightPlain += "  "
		}
		rightPlain += "v" + strings.TrimPrefix(m.version, "v")
	}

	help := composeHelpLine(text, rightPlain, m.termWidth, dim, green, m.updateAvailable)
	return m.renderTable() + "\n" + help
}

// composeHelpLine lays out the bottom status line: left is the
// contextual help/status text, right is the version (optionally
// preceded by the green upgrade notice), right-aligned to the
// terminal's far edge. Per the requirement that the version/upgrade
// notice always stay on one line, the LEFT text is truncated (with a
// trailing "…") if there isn't room for both — the right-hand segment
// is short and important enough to never be the one sacrificed.
// termWidth <= 0 (not yet known) skips alignment entirely and just
// concatenates both halves, consistent with how the rest of the UI
// treats an unknown terminal size as "don't truncate anything yet".
func composeHelpLine(left, rightPlain string, termWidth int, dim, green lipgloss.Style, upgradeAvailable bool) string {
	if rightPlain == "" {
		return dim.Render(left)
	}
	if termWidth <= 0 {
		return dim.Render(left) + "  " + renderRight(rightPlain, upgradeAvailable, dim, green)
	}

	rightWidth := lipgloss.Width(rightPlain)
	avail := termWidth - rightWidth - 2 // 2-space gap between left and right
	if avail < 0 {
		avail = 0
	}
	if lipgloss.Width(left) > avail {
		left = truncateToWidth(left, avail)
	}

	leftRendered := dim.Render(left)
	rightRendered := renderRight(rightPlain, upgradeAvailable, dim, green)

	gap := termWidth - lipgloss.Width(left) - rightWidth
	if gap < 1 {
		gap = 1
	}
	return leftRendered + strings.Repeat(" ", gap) + rightRendered
}

// renderRight styles rightPlain: if it starts with the upgrade notice
// (upgradeAvailable is true), that portion renders green and the
// version portion (after the "  " separator) renders dim; otherwise
// the whole string — just the version — renders dim.
func renderRight(rightPlain string, upgradeAvailable bool, dim, green lipgloss.Style) string {
	if !upgradeAvailable {
		return dim.Render(rightPlain)
	}
	const notice = "press [shift+u] to upgrade"
	if rest, ok := strings.CutPrefix(rightPlain, notice); ok {
		return green.Render(notice) + dim.Render(rest)
	}
	// Shouldn't happen given how rightPlain is built, but fall back to
	// all-dim rather than mis-rendering if it ever does.
	return dim.Render(rightPlain)
}

// truncateToWidth shortens s to at most w visible characters, adding
// a trailing ellipsis if anything was cut. Assumes s is effectively
// single-width per rune (true for this program's help text — plain
// ASCII plus the odd arrow/em-dash), so rune count doubles as display
// width without needing full grapheme-width accounting.
func truncateToWidth(s string, w int) string {
	if w <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	return string(r[:w-1]) + "…"
}

// renderTable builds a fresh lipgloss/table on every call. lipgloss/table
// is a stateless renderer, not a Bubble Tea component, so we rebuild from
// current model state rather than holding a long-lived table instance.
func (m Model) renderTable() string {
	visible := visibleColumns(m.termWidth)
	sparkW := effectiveSparkWidth(m.termWidth)
	fullRows := buildRows(m.visibleIDs(), m.stats, m.history, m.styler, sparkW, m.sparkAscii)

	headers := make([]string, len(visible))
	for i, ci := range visible {
		headers[i] = columns[ci].header
	}
	// Decorate the active sort column's header. The WindowSizeMsg arm
	// already reset sortCol to -1 if the column got hidden, so an arrow
	// here always lands on a visible column.
	if m.sortCol >= 0 {
		for i, ci := range visible {
			if ci == m.sortCol {
				headers[i] += " " + sortArrow(m.sortDesc)
			}
		}
	}
	// Slice the visible window ourselves rather than handing all rows to
	// lipgloss/table with Height()+Offset(). The library reserves the
	// bottom line for an ellipsis when there's overflow, which silently
	// hides the last data row even at maxOffset - on a /24 that's the
	// last two hosts you can never scroll to. By pre-trimming to the
	// window the table never enters overflow mode and every row stays
	// reachable.
	avail := m.visibleRowCount(len(fullRows))
	windowed := fullRows
	if avail < len(fullRows) {
		windowed = fullRows[m.offset : m.offset+avail]
	}
	rows := make([][]string, len(windowed))
	for r, row := range windowed {
		cells := make([]string, len(visible))
		for i, ci := range visible {
			cells[i] = row[ci]
		}
		rows[r] = cells
	}

	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	t := table.New().
		Border(lipgloss.NormalBorder()).
		BorderTop(false).
		BorderBottom(false).
		BorderLeft(false).
		BorderRight(false).
		BorderColumn(false).
		BorderHeader(true).
		BorderStyle(dim).
		Headers(headers...).
		Rows(rows...).
		StyleFunc(func(row, col int) lipgloss.Style {
			origCol := visible[col]
			w := columns[origCol].width
			if origCol == len(columns)-1 {
				w = sparkW + 2
			}
			s := lipgloss.NewStyle().
				Width(w).
				MaxWidth(w).
				PaddingRight(1)
			if row == table.HeaderRow {
				s = s.Bold(true)
			}
			return s
		})
	return t.String()
}

// visibleRowCount is how many data rows fit in the table area.
// Returns total when the terminal height isn't known yet so the first
// frame isn't truncated.
func (m Model) visibleRowCount(total int) int {
	if m.termHeight <= 0 {
		return total
	}
	avail := m.termHeight - 1 - headerLines
	if avail < 1 {
		avail = 1
	}
	if avail > total {
		return total
	}
	return avail
}

// visibleIDs returns m.order filtered by m.filter (case-insensitive
// substring) and then sorted by m.sortCol. When the filter is empty and
// no sort is active, it returns m.order directly.
func (m Model) visibleIDs() []string {
	var out []string
	if m.filter == "" {
		if m.sortCol < 0 {
			return m.order
		}
		out = append(out, m.order...)
	} else {
		f := strings.ToLower(m.filter)
		out = make([]string, 0, len(m.order))
		for _, id := range m.order {
			if strings.Contains(strings.ToLower(id), f) {
				out = append(out, id)
			}
		}
	}
	if m.sortCol < 0 {
		return out
	}
	// TARGET sort works on the ID string and doesn't need stats - keep
	// it out of the missing-stats sink path so every row gets compared.
	if m.sortCol == 0 {
		sort.SliceStable(out, func(i, j int) bool {
			if m.sortDesc {
				return out[i] > out[j]
			}
			return out[i] < out[j]
		})
		return out
	}
	sort.SliceStable(out, func(i, j int) bool {
		si, oki := m.stats[out[i]]
		sj, okj := m.stats[out[j]]
		// Targets without stats sink to the bottom regardless of direction.
		if !oki && !okj {
			return false
		}
		if !oki {
			return false
		}
		if !okj {
			return true
		}
		var less bool
		switch m.sortCol {
		case 1:
			less = si.RTT < sj.RTT
		case 2:
			less = si.MinRTT < sj.MinRTT
		case 3:
			less = si.AvgRTT < sj.AvgRTT
		case 4:
			less = si.MaxRTT < sj.MaxRTT
		case 5:
			less = si.Jitter < sj.Jitter
		case 6:
			less = lossPct(si) < lossPct(sj)
		}
		if m.sortDesc {
			return !less
		}
		return less
	})
	return out
}

// buildRows produces table rows in the stable order. A nil stats map
// renders all targets in their initial "no data yet" state. sparkW is
// the rendered sparkline width (see effectiveSparkWidth).
func buildRows(order []string, stats map[string]pinger.StatsUpdate, history map[string][]time.Duration, st styler, sparkW int, sparkAscii bool) [][]string {
	rows := make([][]string, len(order))
	for i, id := range order {
		s, ok := stats[id]
		if !ok {
			rows[i] = []string{id, "—", "—", "—", "—", "—", "—", "—", "—", formatSpark(nil, sparkW, sparkAscii)}
			continue
		}
		rows[i] = []string{
			id,
			st.render(formatRTT(s), rttLevel(s)),
			formatDur(s.MinRTT, s.Recv, false),
			formatDur(s.AvgRTT, s.Recv, true),
			formatDur(s.MaxRTT, s.Recv, false),
			st.render(formatJitter(s), jitterLevel(s)),
			st.render(formatLoss(s), lossLevel(s)),
			formatSentLost(s),
			formatTTL(s),
			formatSpark(history[id], sparkW, sparkAscii),
		}
	}
	return rows
}

// formatMs renders a Duration always in milliseconds, with no upper
// bound on decimal places (trailing zeros trimmed) — never falling
// back to Duration.String()'s default unit selection, which would
// print exact-zero durations as "0s" (seconds) instead of "0ms". Ping
// RTT/min/max values are always small enough that milliseconds is the
// right unit throughout — including sub-millisecond readings, which
// round to a fractional ms (e.g. 750µs → "0.75ms") rather than
// switching to µs, so every duration in the table shares one
// consistent unit. See formatMsPrecision for the AVG/JITTER variant
// that caps decimal places.
func formatMs(d time.Duration) string {
	ms := float64(d) / float64(time.Millisecond)
	// %g trims trailing zeros (5ms, not 5.000ms) while still printing
	// fractional values (0.75ms) when needed.
	return strconv.FormatFloat(ms, 'g', -1, 64) + "ms"
}

// formatMsPrecision is formatMs capped at decimals decimal places.
// AVG and JITTER are running/smoothed values (a mean, an RFC 3550
// EWMA) that can otherwise carry long floating-point tails with no
// real precision behind them — capping keeps the column readable
// without implying false precision.
func formatMsPrecision(d time.Duration, decimals int) string {
	ms := float64(d) / float64(time.Millisecond)
	return strconv.FormatFloat(ms, 'f', decimals, 64) + "ms"
}

// formatRTT reports the most recent RTT, or a placeholder if no reply
// has arrived yet. Gated on s.Recv rather than s.RTT > 0: on Windows,
// IcmpSendEcho reports RTT in whole milliseconds, so a fast reply
// (same-host, LAN) legitimately rounds to 0ms — treating RTT == 0 as
// "no data" would permanently hide a real, valid 0ms reading behind
// the placeholder. Recv is the actual "has at least one reply arrived"
// signal; RTT's numeric value is not.
func formatRTT(s pinger.StatsUpdate) string {
	if s.Recv > 0 {
		return formatMs(s.RTT)
	}
	if s.LastErr != nil {
		return "err"
	}
	return "—"
}

// formatJitter mirrors formatRTT's Recv-gating logic. Jitter is only
// meaningful from the second reply onward (see the RFC 3550 smoothing
// in run_windows.go/run_unix.go), so Recv < 2 still shows the
// placeholder even though Recv > 0 already gates formatRTT/formatDur.
// Capped at 2 decimal places — see formatMsPrecision's doc.
func formatJitter(s pinger.StatsUpdate) string {
	if s.Recv >= 2 {
		return formatMsPrecision(s.Jitter, 2)
	}
	return "—"
}

// formatDur renders a Duration in milliseconds (see formatMs), gated
// on recv (the target's total successful reply count) rather than the
// duration's own value — see formatRTT's doc for why: a genuinely 0ms
// MIN/AVG/MAX (fast LAN target, Windows' whole-millisecond rounding)
// must still display "0ms", not fall back to the placeholder as if
// no reply had arrived. Used by the MIN/AVG/MAX columns, which have
// no error branch of their own (RTT/JITTER's LastErr handling doesn't
// apply here — those columns just track magnitude over time). avgPrecision
// caps AVG at 2 decimal places (see formatMsPrecision's doc); MIN/MAX
// pass avgPrecision=false and keep formatMs's uncapped trailing-zero-
// trimmed rendering.
func formatDur(d time.Duration, recv int64, avgPrecision bool) string {
	if recv == 0 {
		return "—"
	}
	if avgPrecision {
		return formatMsPrecision(d, 2)
	}
	return formatMs(d)
}

func formatLoss(s pinger.StatsUpdate) string {
	if s.Sent == 0 {
		return "—"
	}
	pct := 100 * float64(s.Sent-s.Recv) / float64(s.Sent)
	return fmt.Sprintf("%.1f%%", pct)
}

// lossPct returns the loss percentage for sort comparison. Returns -1
// when Sent==0 so "no data" rows don't tie zero-loss winners.
func lossPct(s pinger.StatsUpdate) float64 {
	if s.Sent == 0 {
		return -1
	}
	return 100 * float64(s.Sent-s.Recv) / float64(s.Sent)
}

func formatSentLost(s pinger.StatsUpdate) string {
	if s.Sent == 0 {
		return "—"
	}
	lost := s.Sent - s.Recv
	if lost < 0 {
		lost = 0
	}
	return fmt.Sprintf("%d/%d", s.Sent, lost)
}

// formatTTL reports the TTL the most recent reply came back with,
// gated on Recv like formatRTT/formatDur — TTL is only meaningful
// once at least one reply has actually arrived.
func formatTTL(s pinger.StatsUpdate) string {
	if s.Recv > 0 {
		return fmt.Sprintf("%d", s.TTL)
	}
	return "—"
}

// level classifies a metric value into a color bucket. levelNeutral
// means "no data" / "no verdict" - styler renders it without color.
type level int

const (
	levelNeutral level = iota
	levelGood
	levelWarn
	levelCrit
)

// Threshold defaults: chosen to match common sysadmin intuition for
// LAN/WAN ping monitoring. Not configurable in v0.10; promote to flags
// if anyone asks.
const (
	rttWarn     = 50 * time.Millisecond
	rttCrit     = 200 * time.Millisecond
	jitterWarn  = 5 * time.Millisecond
	jitterCrit  = 20 * time.Millisecond
	lossCritPct = 5.0 // ≥ 5% loss is crit; anything > 0 and < 5 is warn
)

// rttLevel is gated on Recv rather than RTT > 0 for the same reason
// formatRTT is — see its doc. A genuinely 0ms reply must still
// classify as "good", not fall through to "no data yet". TimedOut
// forces levelCrit regardless of the stale RTT's magnitude: the
// number on screen is the last known reading, not a fresh "good"
// measurement, and must read as critical until a real reply arrives
// again — see TimedOut's doc on StatsUpdate.
func rttLevel(s pinger.StatsUpdate) level {
	if s.TimedOut {
		return levelCrit
	}
	if s.Recv > 0 {
		switch {
		case s.RTT < rttWarn:
			return levelGood
		case s.RTT < rttCrit:
			return levelWarn
		default:
			return levelCrit
		}
	}
	if s.LastErr != nil {
		return levelCrit
	}
	return levelNeutral
}

// jitterLevel is gated on Recv >= 2 (jitter needs a second reply to
// mean anything — see formatJitter's doc) rather than Jitter > 0, for
// the same 0ms-is-a-real-value reason as rttLevel. TimedOut forces
// levelCrit for the same reason as rttLevel — the displayed jitter is
// stale, not a fresh reading.
func jitterLevel(s pinger.StatsUpdate) level {
	if s.TimedOut {
		return levelCrit
	}
	if s.Recv < 2 {
		return levelNeutral
	}
	switch {
	case s.Jitter < jitterWarn:
		return levelGood
	case s.Jitter < jitterCrit:
		return levelWarn
	default:
		return levelCrit
	}
}

func lossLevel(s pinger.StatsUpdate) level {
	if s.Sent == 0 {
		return levelNeutral
	}
	pct := 100 * float64(s.Sent-s.Recv) / float64(s.Sent)
	switch {
	case pct == 0:
		return levelGood
	case pct < lossCritPct:
		return levelWarn
	default:
		return levelCrit
	}
}

// styler wraps cell strings in lipgloss color styles. The zero value
// (enabled=false) is a no-op renderer, so passing styler{} disables
// coloring without special-casing callers.
type styler struct {
	enabled          bool
	good, warn, crit lipgloss.Style
}

func newStyler(enabled bool) styler {
	if !enabled {
		return styler{}
	}
	return styler{
		enabled: true,
		good:    lipgloss.NewStyle().Foreground(lipgloss.Color("10")),
		warn:    lipgloss.NewStyle().Foreground(lipgloss.Color("11")),
		crit:    lipgloss.NewStyle().Foreground(lipgloss.Color("9")),
	}
}

func (st styler) render(s string, l level) string {
	if !st.enabled || l == levelNeutral {
		return s
	}
	switch l {
	case levelGood:
		return st.good.Render(s)
	case levelWarn:
		return st.warn.Render(s)
	case levelCrit:
		return st.crit.Render(s)
	}
	return s
}

// headerLines counts the rows lipgloss/table renders above the data:
// one for the header titles and one for the border-bottom separator.
const headerLines = 2

// maxOffset is the highest offset that still keeps the last row in
// view. Below this, every row is visible without scrolling.
func (m Model) maxOffset() int {
	if m.termHeight <= 0 {
		return 0
	}
	// Available data rows = termHeight - help line - header lines.
	avail := m.termHeight - 1 - headerLines
	if avail < 1 {
		avail = 1
	}
	rows := len(m.visibleIDs())
	if rows <= avail {
		return 0
	}
	return rows - avail
}

// clampOffset reins offset back in after the visible set or terminal
// height changes (filter edit, dropped row, window resize). Called from
// Update; safe to invoke whenever m.offset might have gone stale.
func clampOffset(m *Model) {
	if max := m.maxOffset(); m.offset > max {
		m.offset = max
	}
	if m.offset < 0 {
		m.offset = 0
	}
}

// visibleColumns returns the indices into `columns` that fit within
// termWidth, dropping by tier (highest tier first) until they fit or
// only tier-0 columns remain. termWidth <= 0 means the program hasn't
// received a WindowSizeMsg yet; render every column so the first frame
// isn't accidentally truncated.
func visibleColumns(termWidth int) []int {
	if termWidth <= 0 {
		idx := make([]int, len(columns))
		for i := range idx {
			idx[i] = i
		}
		return idx
	}
	hidden := make(map[int]bool)
	total := func() int {
		sum := 0
		for i, c := range columns {
			if !hidden[i] {
				sum += c.width
			}
		}
		return sum
	}
	maxTier := 0
	for _, c := range columns {
		if c.tier > maxTier {
			maxTier = c.tier
		}
	}
	for tier := maxTier; tier > 0 && total() > termWidth; tier-- {
		for i, c := range columns {
			if c.tier == tier {
				hidden[i] = true
			}
		}
	}
	out := make([]int, 0, len(columns))
	for i := range columns {
		if !hidden[i] {
			out = append(out, i)
		}
	}
	return out
}

func sortName(col int) string {
	if col < 0 || col >= len(columns) {
		return ""
	}
	return strings.ToLower(columns[col].header)
}

func sortArrow(desc bool) string {
	if desc {
		return "↓"
	}
	return "↑"
}

// visibleSortable returns the indices of columns that are both
// currently visible (per termWidth) and marked sortable.
func visibleSortable(termWidth int) []int {
	out := []int{}
	for _, ci := range visibleColumns(termWidth) {
		if columns[ci].sortable {
			out = append(out, ci)
		}
	}
	return out
}

// nextSortCol advances current through sortable, wrapping to -1 after
// the last entry. If current is -1 or not in sortable (because it just
// got hidden), restart at the first entry.
func nextSortCol(current int, sortable []int) int {
	if len(sortable) == 0 {
		return -1
	}
	for i, ci := range sortable {
		if ci == current {
			if i+1 < len(sortable) {
				return sortable[i+1]
			}
			return -1
		}
	}
	return sortable[0]
}

// prevSortCol walks sortable in reverse, mirroring nextSortCol. From -1
// (or a hidden column) it jumps to the last entry; from the first entry
// it wraps back to -1.
func prevSortCol(current int, sortable []int) int {
	if len(sortable) == 0 {
		return -1
	}
	for i, ci := range sortable {
		if ci == current {
			if i > 0 {
				return sortable[i-1]
			}
			return -1
		}
	}
	return sortable[len(sortable)-1]
}

// columnVisible reports whether the given column index is currently on
// screen given termWidth.
func columnVisible(col, termWidth int) bool {
	for _, ci := range visibleColumns(termWidth) {
		if ci == col {
			return true
		}
	}
	return false
}

func removeID(order []string, id string) []string {
	for i, s := range order {
		if s == id {
			return append(order[:i], order[i+1:]...)
		}
	}
	return order
}

// sparkWidth is the default number of recent RTT samples shown in the
// SPARK column. At a 1 s interval this is also the seconds of visible
// history. On wide terminals the column expands up to maxSparkWidth to
// claim leftover horizontal space.
const sparkWidth = 20
const maxSparkWidth = 200

// sparkBarsUnicode is the 8-level Unicode eighth-block bar set used to
// render samples by default. sparkBarsASCII is the fallback density
// scale for terminals/fonts where the Unicode blocks render as boxes
// (see sparkAscii's doc on Model) — same 8 levels, low to high
// density, but every character is guaranteed present in any font.
var (
	sparkBarsUnicode = []rune("▁▂▃▄▅▆▇█")
	sparkBarsASCII   = []rune(".:-=+*#@")
)

func appendHistory(h map[string][]time.Duration, id string, rtt time.Duration) {
	buf := h[id]
	if len(buf) >= maxSparkWidth {
		buf = buf[1:]
	}
	h[id] = append(buf, rtt)
}

// formatSpark renders the recent RTT samples as a bar chart, scaled
// per-target between the window's min and max so relative jitter is
// what's visible. Pads with leading spaces until the buffer fills, so
// the latest sample is always at the right edge. width sets the
// rendered column width (number of bar cells). ascii selects
// sparkBarsASCII over the Unicode default — see the "t" key toggle in
// Update and sparkAscii's doc on Model.
func formatSpark(history []time.Duration, width int, ascii bool) string {
	bars := sparkBarsUnicode
	if ascii {
		bars = sparkBarsASCII
	}
	if width <= 0 {
		width = sparkWidth
	}
	if len(history) == 0 {
		return strings.Repeat(" ", width)
	}
	if len(history) > width {
		history = history[len(history)-width:]
	}
	min, max := history[0], history[0]
	for _, d := range history[1:] {
		if d < min {
			min = d
		}
		if d > max {
			max = d
		}
	}
	rng := max - min

	var b strings.Builder
	b.Grow(width * 4) // Unicode bars are up to 3-byte UTF-8 runes
	for i := 0; i < width-len(history); i++ {
		b.WriteByte(' ')
	}
	for _, d := range history {
		idx := len(bars) / 2
		if rng > 0 {
			idx = int(int64(d-min) * int64(len(bars)-1) / int64(rng))
			if idx < 0 {
				idx = 0
			}
			if idx >= len(bars) {
				idx = len(bars) - 1
			}
		}
		b.WriteRune(bars[idx])
	}
	return b.String()
}

// effectiveSparkWidth returns how many sparkline bars the SPARK column
// should render given the current terminal width. It claims any
// horizontal slack left over after the visible columns lay out, capped
// at maxSparkWidth. Returns sparkWidth (the default) when SPARK isn't
// visible or termWidth hasn't been received yet.
func effectiveSparkWidth(termWidth int) int {
	if termWidth <= 0 {
		return sparkWidth
	}
	sparkIdx := len(columns) - 1
	visible := visibleColumns(termWidth)
	sparkOn := false
	used := 0
	for _, ci := range visible {
		used += columns[ci].width
		if ci == sparkIdx {
			sparkOn = true
		}
	}
	if !sparkOn {
		return sparkWidth
	}
	slack := termWidth - used
	if slack <= 0 {
		return sparkWidth
	}
	w := sparkWidth + slack
	if w > maxSparkWidth {
		return maxSparkWidth
	}
	return w
}
