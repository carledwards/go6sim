// The Monitor REPL provider. Renders a scrollback area on top + a
// single-line input strip below; routes command submissions through a
// dispatcher; supports bash-style history navigation, scrollback
// paging, and a self-rendered blinking cursor. Same code drives
// cmd/6502-control (wire-client target) and cmd/6502-sim /
// cmd/6502-wasm (HubDirect target).
package monitor

import (
	"strings"
	"sync"
	"time"

	foxpro "github.com/carledwards/foxpro-go"
	"github.com/gdamore/tcell/v2"

	"github.com/carledwards/go6sim/bridge"
)

// EventLine is one line in the scrollback. The kind drives both the
// prefix glyph and the rendered style — see prefixStyle.
type EventLine struct {
	T    time.Time
	Kind string // "input" | "result" | "system" | "info" | "warn" | "halt" | "bp" | "tap" | "err"
	Text string
}

// Monitor is the foxpro ContentProvider that hosts the REPL. Owns
// the scrollback storage, input edit state, command history, and
// scrollback navigation. Application-specific machine metadata +
// quit/help-window plumbing live on the Host. The transport surface
// (CPU.state, mem.peek, clock.run, etc.) lives on the Target.
type Monitor struct {
	foxpro.ScrollState

	host   Host
	target bridge.Target

	// Scrollback storage with bounded capacity. mu guards both the
	// events list and the input/history edit state — the foxpro
	// Draw goroutine and any goroutine doing AddEvent (notification
	// consumers, heal loops) both touch these fields.
	mu        sync.Mutex
	events    []EventLine
	eventsCap int

	input   []rune
	cursor  int
	scrollX int

	history []string
	histIdx int    // -1 = not navigating
	pending []rune // input saved while in history-nav mode
}

// New constructs a Monitor bound to a Host + Target. Set
// eventsCap > 0 to bound the scrollback ring; defaults to 1000 lines
// when zero/negative is passed.
func New(host Host, target bridge.Target) *Monitor {
	return &Monitor{
		host:      host,
		target:    target,
		eventsCap: 1000,
		histIdx:   -1,
	}
}

// AddEvent appends a line to the scrollback. Safe to call from any
// goroutine — notification consumers, heal loops, and the dispatch
// thread all funnel through here.
func (m *Monitor) AddEvent(kind, text string) {
	m.mu.Lock()
	m.events = append(m.events, EventLine{T: time.Now(), Kind: kind, Text: text})
	if len(m.events) > m.eventsCap {
		m.events = m.events[len(m.events)-m.eventsCap:]
	}
	m.mu.Unlock()
}

// Clear empties the scrollback (matches the `cls` / `clear` command
// and a View-menu Clear action). Adds a small system event so the
// user sees something happened.
func (m *Monitor) Clear() {
	m.mu.Lock()
	m.events = m.events[:0]
	m.mu.Unlock()
	m.AddEvent("system", "(scrollback cleared)")
}

// snapshot returns an immutable copy of the scrollback for the Draw
// path. Internal — Draw is the only consumer.
func (m *Monitor) snapshot() []EventLine {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]EventLine, len(m.events))
	copy(out, m.events)
	return out
}

// Draw paints the scrollback (top H-1 rows) + input strip (bottom).
// Follow-latest behaviour: if the user was at the bottom of the
// scrollback before this frame, snap to the new bottom after new
// events arrive. Otherwise leave their offset put so reading older
// context isn't yanked from under them.
func (m *Monitor) Draw(s tcell.Screen, inner foxpro.Rect, th foxpro.Theme, focused bool) {
	if inner.H < 2 || inner.W < 4 {
		return
	}
	sbRows := inner.H - 1
	sbInner := foxpro.Rect{X: inner.X, Y: inner.Y, W: inner.W, H: sbRows}
	inputInner := foxpro.Rect{X: inner.X, Y: inner.Y + sbRows, W: inner.W, H: 1}

	_, prevNatH := m.ContentSize()
	_, prevView := m.LastViewport()
	prevMaxY := prevNatH - prevView
	if prevMaxY < 0 {
		prevMaxY = 0
	}
	_, curY := m.ScrollOffset()
	wasAtBottom := curY >= prevMaxY

	evs := m.snapshot()

	c := foxpro.NewCanvas(s, sbInner, &m.ScrollState)
	style := th.WindowBG
	for i, e := range evs {
		prefix, lineStyle := prefixStyle(e.Kind, style)
		c.Put(0, i, prefix+e.Text, lineStyle)
	}

	_, natH := m.ContentSize()
	newMaxY := natH - sbRows
	if newMaxY < 0 {
		newMaxY = 0
	}
	if wasAtBottom && newMaxY != curY {
		m.SetScrollOffset(0, newMaxY)
	}

	m.drawInput(s, inputInner, th, focused)
}

// drawInput renders the bottom input row in its own distinct style
// (white-on-blue with a yellow "> " prompt) plus a self-rendered
// blinking white cursor — terminal-independent so every host sees
// the same caret.
func (m *Monitor) drawInput(s tcell.Screen, area foxpro.Rect, th foxpro.Theme, focused bool) {
	bg := th.Palette.Blue
	fg := th.Palette.White
	promptStyle := tcell.StyleDefault.Background(bg).Foreground(th.Palette.Yellow)
	textStyle := tcell.StyleDefault.Background(bg).Foreground(fg)

	if area.W < 3 {
		return
	}
	s.SetContent(area.X, area.Y, '>', nil, promptStyle)
	s.SetContent(area.X+1, area.Y, ' ', nil, promptStyle)

	bodyX := area.X + 2
	bodyW := area.W - 2

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cursor < m.scrollX {
		m.scrollX = m.cursor
	} else if m.cursor >= m.scrollX+bodyW {
		m.scrollX = m.cursor - bodyW + 1
	}
	if m.scrollX < 0 {
		m.scrollX = 0
	}

	for x := bodyX; x < bodyX+bodyW; x++ {
		s.SetContent(x, area.Y, ' ', nil, textStyle)
	}
	for i := 0; i < bodyW && m.scrollX+i < len(m.input); i++ {
		s.SetContent(bodyX+i, area.Y, m.input[m.scrollX+i], nil, textStyle)
	}

	if !focused {
		s.HideCursor()
		return
	}

	cx := bodyX + (m.cursor - m.scrollX)
	if cx < bodyX {
		cx = bodyX
	}
	if cx >= bodyX+bodyW {
		cx = bodyX + bodyW - 1
	}

	blinkOn := time.Now().UnixMilli()/500%2 == 0
	if blinkOn {
		cursorStyle := tcell.StyleDefault.
			Background(th.Palette.White).
			Foreground(th.Palette.Blue)
		var ch rune = ' '
		if m.cursor < len(m.input) {
			ch = m.input[m.cursor]
		}
		s.SetContent(cx, area.Y, ch, nil, cursorStyle)
	}
	s.HideCursor()
}

// HandleKey routes editing, history, scrollback nav, and submit.
func (m *Monitor) HandleKey(ev *tcell.EventKey) bool {
	switch ev.Key() {
	case tcell.KeyUp:
		m.historyBack()
		return true
	case tcell.KeyDown:
		m.historyForward()
		return true
	case tcell.KeyPgUp:
		_, curY := m.ScrollOffset()
		m.SetScrollOffset(0, curY-m.halfPage())
		return true
	case tcell.KeyPgDn:
		_, curY := m.ScrollOffset()
		m.SetScrollOffset(0, curY+m.halfPage())
		return true
	case tcell.KeyEnter:
		m.submit()
		return true
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		m.mu.Lock()
		if m.cursor > 0 {
			m.input = append(m.input[:m.cursor-1], m.input[m.cursor:]...)
			m.cursor--
			m.resetHistoryNavLocked()
		}
		m.mu.Unlock()
		return true
	case tcell.KeyDelete:
		m.mu.Lock()
		if m.cursor < len(m.input) {
			m.input = append(m.input[:m.cursor], m.input[m.cursor+1:]...)
			m.resetHistoryNavLocked()
		}
		m.mu.Unlock()
		return true
	case tcell.KeyLeft:
		m.mu.Lock()
		if m.cursor > 0 {
			m.cursor--
		}
		m.mu.Unlock()
		return true
	case tcell.KeyRight:
		m.mu.Lock()
		if m.cursor < len(m.input) {
			m.cursor++
		}
		m.mu.Unlock()
		return true
	case tcell.KeyHome:
		m.mu.Lock()
		m.cursor = 0
		m.mu.Unlock()
		return true
	case tcell.KeyEnd:
		m.mu.Lock()
		m.cursor = len(m.input)
		m.mu.Unlock()
		return true
	case tcell.KeyEscape:
		m.mu.Lock()
		had := len(m.input) > 0
		if had {
			m.input = m.input[:0]
			m.cursor = 0
			m.scrollX = 0
			m.resetHistoryNavLocked()
		}
		m.mu.Unlock()
		return had
	case tcell.KeyRune:
		m.mu.Lock()
		m.input = append(m.input[:m.cursor], append([]rune{ev.Rune()}, m.input[m.cursor:]...)...)
		m.cursor++
		m.resetHistoryNavLocked()
		m.mu.Unlock()
		return true
	}
	return false
}

func (m *Monitor) halfPage() int {
	_, h := m.LastViewport()
	if h > 2 {
		return h / 2
	}
	return 1
}

// submit fires when Enter is pressed: trim, echo, append to history,
// snap scrollback to bottom, hand the line off to dispatch.
func (m *Monitor) submit() {
	m.mu.Lock()
	line := strings.TrimRight(string(m.input), " \t")
	if line == "" {
		m.mu.Unlock()
		return
	}
	m.history = append(m.history, line)
	m.histIdx = -1
	m.pending = nil
	m.SetScrollOffset(0, 1<<30) // snap to bottom; clamped by ScrollState
	m.input = m.input[:0]
	m.cursor = 0
	m.scrollX = 0
	m.mu.Unlock()

	m.AddEvent("input", line)
	m.dispatch(line)
}

func (m *Monitor) resetHistoryNavLocked() {
	if m.histIdx >= 0 {
		m.histIdx = -1
		m.pending = nil
	}
}

func (m *Monitor) historyBack() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.history) == 0 {
		return
	}
	if m.histIdx < 0 {
		m.pending = append(m.pending[:0], m.input...)
		m.histIdx = len(m.history) - 1
	} else if m.histIdx > 0 {
		m.histIdx--
	}
	m.setInputLocked(m.history[m.histIdx])
}

func (m *Monitor) historyForward() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.histIdx < 0 {
		return
	}
	if m.histIdx < len(m.history)-1 {
		m.histIdx++
		m.setInputLocked(m.history[m.histIdx])
		return
	}
	m.histIdx = -1
	m.input = append(m.input[:0], m.pending...)
	m.cursor = len(m.input)
	m.pending = nil
}

func (m *Monitor) setInputLocked(s string) {
	m.input = m.input[:0]
	for _, r := range s {
		m.input = append(m.input, r)
	}
	m.cursor = len(m.input)
}

// prefixStyle picks the leading prefix + tcell style for each event
// kind. Errors render INVERTED (white text on red bg) so they pop
// against the cyan WindowBG — red FG alone on cyan had poor contrast
// at a glance.
func prefixStyle(kind string, base tcell.Style) (string, tcell.Style) {
	switch kind {
	case "input":
		return "> ", base.Bold(true)
	case "result":
		return "  ", base
	case "system":
		return "i ", base.Dim(true)
	case "info":
		return "  ", base.Dim(true)
	case "warn":
		return "⚠ ", base.Foreground(tcell.ColorYellow).Bold(true)
	case "halt", "bp":
		return "* ", base.Foreground(tcell.ColorYellow).Bold(true)
	case "tap":
		return "* ", base.Foreground(tcell.ColorAqua)
	case "err":
		return "! ", tcell.StyleDefault.Background(tcell.ColorMaroon).Foreground(tcell.ColorWhite).Bold(true)
	}
	return "  ", base
}
