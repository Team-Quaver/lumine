package layout

import (
	"math"
	"testing"
)

func newTree() *Tree {
	t := New(8, 0.5)
	t.ViewW, t.ViewH = 1000, 800
	t.DecorH = 28
	return t
}

// TestTileOpensColumns verifies niri-style column spawning: every new window
// gets its own column, placed to the right of the focused one, and the strip
// scrolls horizontally.
func TestTileOpensColumns(t *testing.T) {
	tree := newTree()
	a := tree.Add("a", "app")
	b := tree.Add("b", "app")
	c := tree.Add("c", "app")

	if got := len(tree.columns); got != 3 {
		t.Fatalf("want 3 columns, got %d", got)
	}
	// Focused window is the most recently added one.
	if tree.Focused().ID != c.ID {
		t.Fatalf("focus = %d, want %d", tree.FocusedID(), c.ID)
	}
	tree.Arrange()

	// Columns tile left to right with a gap; each is half the viewport wide.
	cw := tree.columnWidth(tree.columns[0])
	if math.Abs(cw-(1000*0.5-8)) > 0.001 {
		t.Fatalf("column width = %v", cw)
	}
	if got := tree.columns[0].Wins[0].Rect.X; got != 0 {
		t.Fatalf("first column x = %v, want 0", got)
	}
	if got := tree.columns[1].Wins[0].Rect.X; math.Abs(got-(cw+8)) > 0.001 {
		t.Fatalf("second column x = %v, want %v", got, cw+8)
	}
	_ = a
	_ = b
}

// TestTileScrollClamps checks ScrollBy pans the strip and cannot run off either
// end.
func TestTileScrollClamps(t *testing.T) {
	tree := newTree()
	for i := 0; i < 4; i++ {
		tree.Add("w", "app")
	}
	tree.Arrange()

	tree.ScrollBy(1000)
	if tree.scroll < 0 || tree.scroll > tree.maxScroll() {
		t.Fatalf("scroll %v out of range [0,%v]", tree.scroll, tree.maxScroll())
	}
	tree.ScrollBy(-1e6)
	if tree.scroll != 0 {
		t.Fatalf("scroll should clamp to 0, got %v", tree.scroll)
	}
}

// TestTileFocusWalksColumns checks focus-left/right move between columns.
func TestTileFocusWalksColumns(t *testing.T) {
	tree := newTree()
	a := tree.Add("a", "app")
	b := tree.Add("b", "app")
	c := tree.Add("c", "app")
	tree.Arrange()

	if id := tree.Focus("left"); id != b.ID {
		t.Fatalf("focus left = %d, want %d", id, b.ID)
	}
	if id := tree.Focus("left"); id != a.ID {
		t.Fatalf("focus left = %d, want %d", id, a.ID)
	}
	// Already at the left edge: stays put.
	if id := tree.Focus("left"); id != a.ID {
		t.Fatalf("focus left at edge = %d, want %d", id, a.ID)
	}
	if id := tree.Focus("right"); id != b.ID {
		t.Fatalf("focus right = %d, want %d", id, b.ID)
	}
	_ = c
}

// TestTileMoveReordersColumns verifies Move swaps the focused column with its
// neighbour and keeps focus on it.
func TestTileMoveReordersColumns(t *testing.T) {
	tree := newTree()
	a := tree.Add("a", "app")
	b := tree.Add("b", "app")
	// b is focused (right-most). Move it left: order becomes b, a.
	tree.Move("left")
	if tree.columns[0].Wins[0].ID != b.ID || tree.columns[1].Wins[0].ID != a.ID {
		t.Fatalf("columns not reordered: %d,%d", tree.columns[0].Wins[0].ID, tree.columns[1].Wins[0].ID)
	}
	if tree.FocusedID() != b.ID {
		t.Fatalf("focus lost after move: %d", tree.FocusedID())
	}
	// At the left edge a further Move-left is a no-op.
	tree.Move("left")
	if tree.columns[0].Wins[0].ID != b.ID {
		t.Fatalf("move past edge changed order")
	}
}

// TestTileRemoveCollapsesColumn checks removing the only window of a column
// drops the column and keeps focus valid.
func TestTileRemoveCollapsesColumn(t *testing.T) {
	tree := newTree()
	a := tree.Add("a", "app")
	b := tree.Add("b", "app")
	tree.Arrange()

	if !tree.Remove(b.ID) {
		t.Fatal("Remove returned false")
	}
	if len(tree.columns) != 1 {
		t.Fatalf("want 1 column after removal, got %d", len(tree.columns))
	}
	if tree.FocusedID() != a.ID {
		t.Fatalf("focus = %d, want %d", tree.FocusedID(), a.ID)
	}
	if tree.Remove(9999) {
		t.Fatal("Remove of unknown id returned true")
	}
}

// TestSetModePreservesWindows checks a round trip through stack mode keeps
// every window and that stack focus is the top of the z-order.
func TestSetModePreservesWindows(t *testing.T) {
	tree := newTree()
	a := tree.Add("a", "app")
	b := tree.Add("b", "app")
	c := tree.Add("c", "app")

	tree.SetMode(ModeStack)
	if tree.Mode != ModeStack {
		t.Fatal("mode not switched to stack")
	}
	if got := len(tree.Windows()); got != 3 {
		t.Fatalf("stack windows = %d, want 3", got)
	}
	if tree.FocusedID() != c.ID {
		t.Fatalf("stack focus = %d, want %d", tree.FocusedID(), c.ID)
	}
	// Focus left lowers the top window; focus returns to the new top.
	tree.Focus("left")
	if tree.FocusedID() == c.ID {
		t.Fatal("focus left did not cycle the stack")
	}
	if id := tree.SetFocus(a.ID); !id || tree.FocusedID() != a.ID {
		t.Fatalf("SetFocus(%d) failed", a.ID)
	}

	tree.SetMode(ModeTile)
	if got := len(tree.Windows()); got != 3 {
		t.Fatalf("tile windows after round trip = %d, want 3", got)
	}
	_ = b
}

// TestStackPlacementStaysOnScreen checks floating windows are clamped inside
// the viewport.
func TestStackPlacementStaysOnScreen(t *testing.T) {
	tree := newTree()
	tree.SetMode(ModeStack)
	w := tree.Add("w", "app")
	w.FreeX, w.FreeY = 5000, 5000
	tree.Arrange()
	if w.Rect.X < 0 || w.Rect.Y < 0 {
		t.Fatalf("window at negative position: %+v", w.Rect)
	}
	if w.Rect.X+w.Rect.W > tree.ViewW+0.001 || w.Rect.Y+w.Rect.H > tree.ViewH+0.001 {
		t.Fatalf("window off screen: rect=%+v view=%vx%v", w.Rect, tree.ViewW, tree.ViewH)
	}
}

// TestTileDecorReservesSpace checks decorated windows get their title bar
// height reserved above the content, and that the column still fits inside the
// viewport (top and bottom gaps included).
func TestTileDecorReservesSpace(t *testing.T) {
	tree := newTree()
	a := tree.Add("a", "app")
	a.Decorated = true
	b := tree.Add("b", "app")
	b.Decorated = true
	tree.Arrange()
	for _, w := range []*Win{a, b} {
		if w.Rect.Y < tree.DecorH {
			t.Fatalf("decorated window y = %v, want >= %v", w.Rect.Y, tree.DecorH)
		}
	}
	if bottom := b.Rect.Y + b.Rect.H; bottom > tree.ViewH+0.001 {
		t.Fatalf("column bottom = %v exceeds viewport %v", bottom, tree.ViewH)
	}
	if top := a.Rect.Y - tree.DecorH; top < -0.001 {
		t.Fatalf("title bar top = %v is above the viewport", top)
	}
}
