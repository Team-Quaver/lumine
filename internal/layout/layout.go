// Package layout implements Lumine's two layout modes:
//
//   - Tile: niri-style infinite horizontal scroll of vertical columns.
//     Columns keep a fixed width; the viewport scrolls along the column
//     strip. Focus moves column-wise / window-wise.
//   - Stack: free-floating windows, raised/lowered in z-order, positioned
//     wherever the user (or the compositor) puts them.
package layout

import (
	"math"
)

// Mode selects the active layout algorithm.
type Mode string

const (
	ModeTile  Mode = "tile"
	ModeStack Mode = "stack"
)

// Rect is a window geometry in output-local coordinates.
type Rect struct {
	X, Y, W, H float64
}

// Win is one managed window. The compositor fills the client-facing fields;
// layout owns geometry.
type Win struct {
	ID        uint64
	Title     string
	AppID     string
	Rect      Rect // current (animated target) geometry
	MinW      int  // client-requested min size (0 = none)
	MinH      int
	MaxW      int
	MaxH      int
	Activated bool
	// Decorated marks windows that get server-side decorations; the layout
	// reserves DecorH pixels above them for the title bar.
	Decorated bool
	// stack-mode position chosen by drag / placement.
	FreeX, FreeY float64
}

// Tree is the layout state for one output.
type Tree struct {
	Mode Mode

	// tile state
	columns  []*Column
	scroll   float64 // horizontal scroll offset in px
	nextID   uint64
	focusCol int
	focusWin int

	// stack state
	stack []*Win

	// tuning
	Gap         float64
	ColumnRatio float64 // fraction of viewport width per column
	ViewW       float64
	ViewH       float64
	DecorH      float64 // title-bar height reserved for decorated windows
}

// Column is one vertical stack of windows in tile mode.
type Column struct {
	Wins []*Win
}

func New(gap float64, colRatio float64) *Tree {
	return &Tree{Mode: ModeTile, Gap: gap, ColumnRatio: colRatio}
}

func (t *Tree) nextWinID() uint64 {
	t.nextID++
	return t.nextID
}

// Add registers a new window. In tile mode it opens in its own column,
// immediately to the right of the focused one (niri's default): windows are
// laid out as a horizontal strip of vertical columns that the viewport scrolls
// across.
func (t *Tree) Add(title, appID string) *Win {
	w := &Win{ID: t.nextWinID(), Title: title, AppID: appID}
	switch t.Mode {
	case ModeStack:
		t.stack = append(t.stack, w)
		w.FreeX = 60 + float64((len(t.stack)*37)%220)
		w.FreeY = 60 + float64((len(t.stack)*29)%160)
	default:
		col := &Column{Wins: []*Win{w}}
		if len(t.columns) == 0 {
			t.columns = append(t.columns, col)
			t.focusCol, t.focusWin = 0, 0
			return w
		}
		t.focusCol++
		if t.focusCol > len(t.columns) {
			t.focusCol = len(t.columns)
		}
		t.columns = append(t.columns, nil)
		copy(t.columns[t.focusCol+1:], t.columns[t.focusCol:])
		t.columns[t.focusCol] = col
		t.focusWin = 0
	}
	return w
}

func (t *Tree) Remove(id uint64) bool {
	switch t.Mode {
	case ModeStack:
		for i, w := range t.stack {
			if w.ID == id {
				t.stack = append(t.stack[:i], t.stack[i+1:]...)
				return true
			}
		}
		return false
	default:
		for ci, col := range t.columns {
			for wi, w := range col.Wins {
				if w.ID != id {
					continue
				}
				col.Wins = append(col.Wins[:wi], col.Wins[wi+1:]...)
				if len(col.Wins) == 0 {
					t.columns = append(t.columns[:ci], t.columns[ci+1:]...)
					if t.focusCol >= len(t.columns) {
						t.focusCol = maxInt(0, len(t.columns)-1)
					}
					t.focusWin = 0
				} else if t.focusCol == ci {
					if t.focusWin >= len(col.Wins) {
						t.focusWin = len(col.Wins) - 1
					}
				}
				return true
			}
		}
		return false
	}
}

func (t *Tree) focusedColumn() *Column {
	if t.focusCol < len(t.columns) {
		return t.columns[t.focusCol]
	}
	return nil
}

// Focused returns the focused window, if any.
func (t *Tree) Focused() *Win {
	switch t.Mode {
	case ModeStack:
		if len(t.stack) == 0 {
			return nil
		}
		return t.stack[len(t.stack)-1] // top of stack
	default:
		col := t.focusedColumn()
		if col == nil || t.focusWin >= len(col.Wins) {
			return nil
		}
		return col.Wins[t.focusWin]
	}
}

// FocusedID returns the focused window id or 0.
func (t *Tree) FocusedID() uint64 {
	if w := t.Focused(); w != nil {
		return w.ID
	}
	return 0
}

// SetFocus moves focus to id. Returns false when unknown.
func (t *Tree) SetFocus(id uint64) bool {
	switch t.Mode {
	case ModeStack:
		for i, w := range t.stack {
			if w.ID == id {
				top := t.stack[len(t.stack)-1]
				t.stack[len(t.stack)-1], t.stack[i] = t.stack[i], top
				return true
			}
		}
		return false
	default:
		for ci, col := range t.columns {
			for wi, w := range col.Wins {
				if w.ID == id {
					t.focusCol, t.focusWin = ci, wi
					return true
				}
			}
		}
		return false
	}
}

// ScrollBy pans the column strip horizontally (niri scroll).
func (t *Tree) ScrollBy(dx float64) {
	if t.Mode != ModeTile {
		return
	}
	t.scroll += dx
	t.clampScroll()
}

func (t *Tree) clampScroll() {
	max := t.maxScroll()
	if t.scroll > max {
		t.scroll = max
	}
	if t.scroll < 0 {
		t.scroll = 0
	}
}

func (t *Tree) maxScroll() float64 {
	if len(t.columns) == 0 {
		return 0
	}
	total := t.stripWidth()
	return math.Max(0, total-t.ViewW)
}

func (t *Tree) stripWidth() float64 {
	total := 0.0
	for _, col := range t.columns {
		total += t.columnWidth(col) + t.Gap
	}
	if total > 0 {
		total -= t.Gap
	}
	return total
}

func (t *Tree) columnWidth(col *Column) float64 {
	w := t.ViewW * t.ColumnRatio
	if w <= 0 {
		w = t.ViewW / 2
	}
	return w - t.Gap
}

// Arrange recomputes geometry for every window in tile mode and lays the
// stack out at its free positions in stack mode. Returns focused id.
func (t *Tree) Arrange() uint64 {
	switch t.Mode {
	case ModeStack:
		for i, w := range t.stack {
			w.Activated = i == len(t.stack)-1
			t.placeStack(w)
		}
	default:
		t.clampScroll()
		x := -t.scroll
		for ci, col := range t.columns {
			cw := t.columnWidth(col)
			// Distribute the viewport height over the column. Decorated
			// windows additionally reserve DecorH for their title bar.
			n := len(col.Wins)
			// Reserve the gaps above/below the column (top+bottom, plus the
			// n-1 gaps between windows) and a title bar per decorated window
			// before splitting the remainder evenly.
			inner := t.ViewH - t.Gap*float64(n+1)
			for _, w := range col.Wins {
				inner -= t.decorH(w)
			}
			h := inner / float64(maxInt(n, 1))
			if h < 1 {
				h = 1
			}
			y := t.Gap
			for wi, w := range col.Wins {
				d := t.decorH(w)
				w.Rect = Rect{X: x, Y: y + d, W: cw, H: h}
				w.Activated = ci == t.focusCol && wi == t.focusWin
				y += d + h + t.Gap
			}
			x += cw + t.Gap
		}
		// keep the focused column reasonably on screen
		if fc := t.focusedColumn(); fc != nil && len(fc.Wins) > 0 {
			fx := fc.Wins[0].Rect.X
			fw := fc.Wins[0].Rect.W
			if fx < 0 {
				t.scroll += fx
			} else if fx+fw > t.ViewW {
				t.scroll += fx + fw - t.ViewW
			}
			t.clampScroll()
		}
	}
	return t.FocusedID()
}

// Focus moves focus in a direction and re-arranges. "left"/"right" walk
// columns, "up"/"down" walk windows within the column. In stack mode,
// left/right cycle the z-order.
func (t *Tree) Focus(dir string) uint64 {
	switch t.Mode {
	case ModeStack:
		if len(t.stack) > 1 {
			top := t.stack[len(t.stack)-1]
			switch dir {
			case "left", "up":
				copy(t.stack[1:], t.stack)
				t.stack[0] = top
			case "right", "down":
				copy(t.stack[0:], t.stack[1:])
				t.stack[len(t.stack)-1] = top
			}
		}
	default:
		switch dir {
		case "left":
			if t.focusCol > 0 {
				t.focusCol--
				t.focusWin = 0
			}
		case "right":
			if t.focusCol < len(t.columns)-1 {
				t.focusCol++
				t.focusWin = 0
			}
		case "up":
			if t.focusWin > 0 {
				t.focusWin--
			}
		case "down":
			col := t.focusedColumn()
			if col != nil && t.focusWin < len(col.Wins)-1 {
				t.focusWin++
			}
		}
	}
	return t.Arrange()
}

// Move reorders the focused column within the strip ("left"/"right"), which is
// how a window is pushed earlier or later in the scroll order. It is a no-op in
// stack mode.
func (t *Tree) Move(dir string) {
	if t.Mode != ModeTile || len(t.columns) == 0 {
		return
	}
	switch dir {
	case "left", "up":
		if t.focusCol > 0 {
			t.columns[t.focusCol-1], t.columns[t.focusCol] = t.columns[t.focusCol], t.columns[t.focusCol-1]
			t.focusCol--
		}
	case "right", "down":
		if t.focusCol < len(t.columns)-1 {
			t.columns[t.focusCol+1], t.columns[t.focusCol] = t.columns[t.focusCol], t.columns[t.focusCol+1]
			t.focusCol++
		}
	}
	t.Arrange()
}

// SetMode switches layout mode, keeping all windows.
func (t *Tree) SetMode(m Mode) {
	if t.Mode == m {
		return
	}
	// Collect all windows from whichever mode we are leaving.
	var wins []*Win
	if t.Mode == ModeStack {
		wins = t.stack
		t.stack = nil
	} else {
		for _, col := range t.columns {
			wins = append(wins, col.Wins...)
		}
		t.columns = nil
	}
	t.Mode = m
	for _, w := range wins {
		t.Adopt(w)
	}
	t.Arrange()
}

// Adopt moves an existing window into the current mode's structure.
func (t *Tree) Adopt(w *Win) {
	switch t.Mode {
	case ModeStack:
		t.stack = append(t.stack, w)
	default:
		col := t.focusedColumn()
		if col == nil {
			col = &Column{}
			t.columns = append(t.columns, col)
			t.focusCol = len(t.columns) - 1
		}
		col.Wins = append(col.Wins, w)
	}
}

// Windows returns every window in draw order (back to front).
func (t *Tree) Windows() []*Win {
	var out []*Win
	switch t.Mode {
	case ModeStack:
		out = append(out, t.stack...)
	default:
		for _, col := range t.columns {
			out = append(out, col.Wins...)
		}
	}
	return out
}

// decorH returns the vertical space a window needs for server-side
// decorations.
func (t *Tree) decorH(w *Win) float64 {
	if w.Decorated {
		return t.DecorH
	}
	return 0
}

// placeStack gives a floating window its first geometry and keeps it on
// screen afterwards (stack mode never re-tiles existing geometry).
func (t *Tree) placeStack(w *Win) {
	if w.Rect.W <= 0 || w.Rect.H <= 0 {
		w.Rect.W = math.Round(t.ViewW * 0.6)
		w.Rect.H = math.Round(t.ViewH * 0.6)
		if w.Rect.W < 1 {
			w.Rect.W = t.ViewW
		}
		if w.Rect.H < 1 {
			w.Rect.H = t.ViewH
		}
		w.Rect.X = w.FreeX
		w.Rect.Y = w.FreeY
	}
	maxX := math.Max(0, t.ViewW-w.Rect.W)
	maxY := math.Max(0, t.ViewH-w.Rect.H)
	w.Rect.X = math.Min(math.Max(0, w.Rect.X), maxX)
	w.Rect.Y = math.Min(math.Max(0, w.Rect.Y), maxY)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
