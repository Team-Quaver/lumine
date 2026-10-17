package wl

// Mods is the modifier state reported by a seat. It lives in the protocol
// package so both the compositor and any backend can share the type without
// importing each other.
type Mods struct {
	Super bool
	Shift bool
	Ctrl  bool
	Alt   bool
}
