// Package sessionprompt turns a session's terminal into something a program
// can read: a minimal VT screen that tracks what a human would currently see,
// and a detector that recognises interactive questions (selection menus,
// Yes/No confirmations) on that screen so they can be exposed and answered
// through the Automation API instead of blocking on a human.
package sessionprompt

import (
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// Screen is a deliberately small terminal emulator. It implements the subset
// of ECMA-48/xterm that TUIs such as Claude Code and Codex use to paint
// (cursor addressing, erase, scroll regions, the alternate screen) and
// ignores everything else. It never has to be pixel-perfect: the detector
// only needs the text layout of the lines a dialog occupies.
type Screen struct {
	mu        sync.Mutex
	rows      int
	cols      int
	main      *grid
	alt       *grid
	active    *grid
	altActive bool

	state   parserState
	params  []byte
	inter   []byte
	pending []byte // incomplete UTF-8 sequence carried across Feed calls
	version uint64
}

type parserState int

const (
	stateGround parserState = iota
	stateEscape
	stateCSI
	stateOSC
	stateOSCEscape
	stateString       // DCS/SOS/PM/APC payload, discarded until ST
	stateStringEscape // ESC seen inside a discarded string
	stateCharset      // ESC ( X and friends: swallow one byte
)

const wideContinuation rune = -1

type grid struct {
	cells       [][]rune
	x, y        int
	savedX      int
	savedY      int
	top, bottom int
	wrapPending bool
}

// NewScreen returns a blank screen of the given size.
func NewScreen(rows, cols int) *Screen {
	rows, cols = clampSize(rows, cols)
	s := &Screen{rows: rows, cols: cols}
	s.main = newGrid(rows, cols)
	s.alt = newGrid(rows, cols)
	s.active = s.main
	return s
}

func clampSize(rows, cols int) (int, int) {
	if rows < 2 {
		rows = 24
	}
	if cols < 10 {
		cols = 80
	}
	if rows > 500 {
		rows = 500
	}
	if cols > 1000 {
		cols = 1000
	}
	return rows, cols
}

func newGrid(rows, cols int) *grid {
	g := &grid{cells: make([][]rune, rows), bottom: rows - 1}
	for i := range g.cells {
		g.cells[i] = blankRow(cols)
	}
	return g
}

func blankRow(cols int) []rune {
	row := make([]rune, cols)
	for i := range row {
		row[i] = ' '
	}
	return row
}

// Version increases on every Feed; callers use it to skip re-detection when
// nothing was painted.
func (s *Screen) Version() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.version
}

// Resize changes the screen size the way a terminal does on SIGWINCH: content
// stays anchored top-left, and rows that would push the cursor off the bottom
// scroll away.
func (s *Screen) Resize(rows, cols int) {
	rows, cols = clampSize(rows, cols)
	s.mu.Lock()
	defer s.mu.Unlock()
	if rows == s.rows && cols == s.cols {
		return
	}
	for _, g := range []*grid{s.main, s.alt} {
		g.resize(s.rows, rows, cols)
	}
	s.rows, s.cols = rows, cols
	s.version++
}

func (g *grid) resize(oldRows, rows, cols int) {
	shift := 0
	if g.y >= rows {
		shift = g.y - rows + 1
	}
	next := make([][]rune, rows)
	for i := range next {
		next[i] = blankRow(cols)
		if src := i + shift; src < oldRows && src < len(g.cells) {
			copy(next[i], g.cells[src])
		}
	}
	g.cells = next
	g.y -= shift
	g.x = min(g.x, cols-1)
	g.y = max(0, min(g.y, rows-1))
	g.top, g.bottom = 0, rows-1
	g.wrapPending = false
}

// Lines returns the visible screen with trailing blanks trimmed, plus the
// cursor row.
func (s *Screen) Lines() ([]string, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.active.cells))
	var b strings.Builder
	for i, row := range s.active.cells {
		b.Reset()
		for _, r := range row {
			if r == wideContinuation {
				continue
			}
			b.WriteRune(r)
		}
		out[i] = strings.TrimRight(b.String(), " ")
	}
	return out, s.active.y
}

// Feed advances the emulator with raw PTY output.
func (s *Screen) Feed(data []byte) {
	if len(data) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version++
	if len(s.pending) > 0 {
		data = append(append([]byte(nil), s.pending...), data...)
		s.pending = s.pending[:0]
	}
	for i := 0; i < len(data); {
		c := data[i]
		switch s.state {
		case stateGround:
			if c >= 0x80 {
				if !utf8.FullRune(data[i:]) {
					s.pending = append(s.pending, data[i:]...)
					return
				}
				r, size := utf8.DecodeRune(data[i:])
				i += size
				if r != utf8.RuneError {
					s.print(r)
				}
				continue
			}
			s.ground(c)
		case stateEscape:
			s.escape(c)
		case stateCSI:
			switch {
			case c >= 0x30 && c <= 0x3f:
				s.params = append(s.params, c)
			case c >= 0x20 && c <= 0x2f:
				s.inter = append(s.inter, c)
			case c >= 0x40 && c <= 0x7e:
				s.csi(c)
				s.state = stateGround
			case c == 0x1b:
				s.state = stateEscape
			case c == 0x18 || c == 0x1a:
				s.state = stateGround
			}
		case stateOSC:
			if c == 0x07 {
				s.state = stateGround
			} else if c == 0x1b {
				s.state = stateOSCEscape
			}
		case stateOSCEscape:
			s.state = stateGround
			if c != '\\' {
				continue // reprocess as a fresh byte in ground
			}
		case stateString:
			if c == 0x1b {
				s.state = stateStringEscape
			} else if c == 0x07 {
				s.state = stateGround
			}
		case stateStringEscape:
			if c == '\\' {
				s.state = stateGround
			} else {
				s.state = stateString
			}
		case stateCharset:
			s.state = stateGround
		}
		i++
	}
}

func (s *Screen) ground(c byte) {
	g := s.active
	switch c {
	case 0x1b:
		s.state = stateEscape
	case '\r':
		g.x = 0
		g.wrapPending = false
	case '\n', '\v', '\f':
		s.lineFeed()
	case '\b':
		if g.x > 0 {
			g.x--
		}
		g.wrapPending = false
	case '\t':
		g.x = min(s.cols-1, (g.x/8+1)*8)
		g.wrapPending = false
	default:
		if c >= 0x20 && c < 0x7f {
			s.print(rune(c))
		}
	}
}

func (s *Screen) escape(c byte) {
	g := s.active
	s.state = stateGround
	switch c {
	case '[':
		s.params = s.params[:0]
		s.inter = s.inter[:0]
		s.state = stateCSI
	case ']':
		s.state = stateOSC
	case 'P', 'X', '^', '_':
		s.state = stateString
	case '(', ')', '*', '+', '-', '.', '/', '#', '%':
		s.state = stateCharset
	case '7':
		g.savedX, g.savedY = g.x, g.y
	case '8':
		g.x, g.y = g.savedX, g.savedY
		g.wrapPending = false
	case 'D':
		s.lineFeed()
	case 'E':
		g.x = 0
		s.lineFeed()
	case 'M':
		if g.y == g.top {
			s.scrollDown(1)
		} else if g.y > 0 {
			g.y--
		}
		g.wrapPending = false
	case 'c':
		s.main = newGrid(s.rows, s.cols)
		s.alt = newGrid(s.rows, s.cols)
		s.active = s.main
		s.altActive = false
	}
}

func (s *Screen) print(r rune) {
	g := s.active
	width := runeWidth(r)
	if width == 0 {
		return
	}
	if g.wrapPending || g.x+width > s.cols {
		g.x = 0
		s.lineFeed()
	}
	g.cells[g.y][g.x] = r
	if width == 2 && g.x+1 < s.cols {
		g.cells[g.y][g.x+1] = wideContinuation
	}
	if g.x+width >= s.cols {
		g.x = s.cols - 1
		g.wrapPending = true
		return
	}
	g.x += width
}

func (s *Screen) lineFeed() {
	g := s.active
	g.wrapPending = false
	if g.y == g.bottom {
		s.scrollUp(1)
		return
	}
	if g.y < s.rows-1 {
		g.y++
	}
}

func (s *Screen) scrollUp(n int) {
	g := s.active
	for ; n > 0; n-- {
		copy(g.cells[g.top:g.bottom], g.cells[g.top+1:g.bottom+1])
		g.cells[g.bottom] = blankRow(s.cols)
	}
}

func (s *Screen) scrollDown(n int) {
	g := s.active
	for ; n > 0; n-- {
		copy(g.cells[g.top+1:g.bottom+1], g.cells[g.top:g.bottom])
		g.cells[g.top] = blankRow(s.cols)
	}
}

func (s *Screen) csiParams() (private byte, values []int) {
	raw := s.params
	if len(raw) > 0 && (raw[0] == '?' || raw[0] == '>' || raw[0] == '<' || raw[0] == '=') {
		private = raw[0]
		raw = raw[1:]
	}
	for _, part := range strings.Split(string(raw), ";") {
		if sub := strings.IndexByte(part, ':'); sub >= 0 {
			part = part[:sub]
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			n = 0
		}
		values = append(values, n)
	}
	return private, values
}

func param(values []int, index, fallback int) int {
	if index < len(values) && values[index] > 0 {
		return values[index]
	}
	return fallback
}

func (s *Screen) csi(final byte) {
	private, values := s.csiParams()
	if len(s.inter) > 0 {
		return // cursor style (SP q), DECSCA etc.: no layout effect
	}
	g := s.active
	if private == '?' {
		if final == 'h' || final == 'l' {
			for _, mode := range values {
				if mode == 1049 || mode == 1047 || mode == 47 {
					s.setAltScreen(final == 'h', mode == 1049)
				}
			}
		}
		return
	}
	if private != 0 {
		return
	}
	n := param(values, 0, 1)
	switch final {
	case 'A':
		g.y = max(g.y-n, 0)
	case 'B', 'e':
		g.y = min(g.y+n, s.rows-1)
	case 'C', 'a':
		g.x = min(g.x+n, s.cols-1)
	case 'D':
		g.x = max(g.x-n, 0)
	case 'E':
		g.y = min(g.y+n, s.rows-1)
		g.x = 0
	case 'F':
		g.y = max(g.y-n, 0)
		g.x = 0
	case 'G', '`':
		g.x = min(n-1, s.cols-1)
	case 'd':
		g.y = min(n-1, s.rows-1)
	case 'H', 'f':
		g.y = min(param(values, 0, 1)-1, s.rows-1)
		g.x = min(param(values, 1, 1)-1, s.cols-1)
	case 'J':
		s.eraseDisplay(firstValue(values))
	case 'K':
		s.eraseLine(firstValue(values))
	case 'X':
		for i := g.x; i < min(g.x+n, s.cols); i++ {
			g.cells[g.y][i] = ' '
		}
	case 'P':
		row := g.cells[g.y]
		n = min(n, s.cols-g.x)
		copy(row[g.x:], row[g.x+n:])
		for i := s.cols - n; i < s.cols; i++ {
			row[i] = ' '
		}
	case '@':
		row := g.cells[g.y]
		n = min(n, s.cols-g.x)
		copy(row[g.x+n:], row[g.x:s.cols-n])
		for i := g.x; i < g.x+n; i++ {
			row[i] = ' '
		}
	case 'L', 'M':
		if g.y < g.top || g.y > g.bottom {
			return
		}
		top := g.top
		g.top = g.y
		if final == 'L' {
			s.scrollDown(min(n, g.bottom-g.y+1))
		} else {
			s.scrollUp(min(n, g.bottom-g.y+1))
		}
		g.top = top
	case 'S':
		s.scrollUp(min(n, g.bottom-g.top+1))
	case 'T':
		s.scrollDown(min(n, g.bottom-g.top+1))
	case 'r':
		top := param(values, 0, 1) - 1
		bottom := param(values, 1, s.rows) - 1
		if top < bottom && bottom < s.rows {
			g.top, g.bottom = top, bottom
		} else {
			g.top, g.bottom = 0, s.rows-1
		}
		g.x, g.y = 0, 0
	case 's':
		g.savedX, g.savedY = g.x, g.y
	case 'u':
		g.x, g.y = g.savedX, g.savedY
	default:
		return
	}
	g.wrapPending = false
}

func firstValue(values []int) int {
	if len(values) == 0 {
		return 0
	}
	return values[0]
}

func (s *Screen) eraseDisplay(mode int) {
	g := s.active
	switch mode {
	case 0:
		s.eraseLine(0)
		for y := g.y + 1; y < s.rows; y++ {
			g.cells[y] = blankRow(s.cols)
		}
	case 1:
		s.eraseLine(1)
		for y := 0; y < g.y; y++ {
			g.cells[y] = blankRow(s.cols)
		}
	case 2, 3:
		for y := range g.cells {
			g.cells[y] = blankRow(s.cols)
		}
	}
}

func (s *Screen) eraseLine(mode int) {
	g := s.active
	row := g.cells[g.y]
	from, to := g.x, s.cols
	switch mode {
	case 1:
		from, to = 0, min(g.x+1, s.cols)
	case 2:
		from, to = 0, s.cols
	}
	for i := from; i < to; i++ {
		row[i] = ' '
	}
}

func (s *Screen) setAltScreen(on, saveCursor bool) {
	if on == s.altActive {
		return
	}
	if on {
		if saveCursor {
			s.main.savedX, s.main.savedY = s.main.x, s.main.y
		}
		s.alt = newGrid(s.rows, s.cols)
		s.active = s.alt
	} else {
		s.active = s.main
		if saveCursor {
			s.main.x, s.main.y = s.main.savedX, s.main.savedY
		}
	}
	s.altActive = on
}

// runeWidth is a compact East-Asian-width approximation: combining marks and
// zero-width joiners take no cell, CJK and emoji take two, everything else
// (including the box-drawing and pointer glyphs TUIs use) takes one.
func runeWidth(r rune) int {
	switch {
	case r == 0 || r == 0x200b || r == 0x200c || r == 0x200d || r == 0xfe0f || r == 0xfe0e:
		return 0
	case r >= 0x0300 && r <= 0x036f, r >= 0x20d0 && r <= 0x20ff:
		return 0
	case r >= 0x1100 && r <= 0x115f, r >= 0x2e80 && r <= 0xa4cf, r >= 0xac00 && r <= 0xd7a3,
		r >= 0xf900 && r <= 0xfaff, r >= 0xfe30 && r <= 0xfe4f, r >= 0xff00 && r <= 0xff60,
		r >= 0xffe0 && r <= 0xffe6, r >= 0x1f300 && r <= 0x1f64f, r >= 0x1f900 && r <= 0x1f9ff,
		r >= 0x20000 && r <= 0x3fffd:
		return 2
	}
	return 1
}
