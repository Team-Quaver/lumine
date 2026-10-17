package drm

// SelectMode chooses a connector mode. When width/height are non-zero only
// modes with exactly that resolution are considered; otherwise the preferred
// mode wins, falling back to the largest mode the connector reports.
//
// refreshMHz selects among same-resolution candidates: the closest refresh
// rate wins, ties broken toward the higher rate. A zero refreshMHz means
// "highest available".
func SelectMode(modes []Mode, width, height int, refreshMHz int32) (Mode, bool) {
	if len(modes) == 0 {
		return Mode{}, false
	}
	cands := make([]Mode, 0, len(modes))
	if width > 0 && height > 0 {
		for _, m := range modes {
			if m.Width() == width && m.Height() == height {
				cands = append(cands, m)
			}
		}
		if len(cands) == 0 {
			return Mode{}, false
		}
	} else {
		var preferred *Mode
		var largest Mode
		for i := range modes {
			m := modes[i]
			if m.Preferred() && preferred == nil {
				preferred = &modes[i]
			}
			if m.Width()*m.Height() > largest.Width()*largest.Height() ||
				(largest.Width() == 0 && largest.HDisplay == 0) {
				largest = m
			}
		}
		if preferred != nil {
			return *preferred, true
		}
		return largest, true
	}
	best := cands[0]
	bestDelta := refreshDelta(best.RefreshMHz(), refreshMHz)
	for _, m := range cands[1:] {
		d := refreshDelta(m.RefreshMHz(), refreshMHz)
		if d < bestDelta || (d == bestDelta && m.VRefresh > best.VRefresh) {
			best, bestDelta = m, d
		}
	}
	return best, true
}

// refreshDelta scores a candidate refresh against the request. A zero request
// asks for the highest rate, so it is scored favourably by absolute rate.
func refreshDelta(have, want int32) int32 {
	if want <= 0 {
		// Prefer higher refresh: invert so larger rates give smaller deltas.
		return 1_000_000 - have
	}
	d := have - want
	if d < 0 {
		d = -d
	}
	return d
}
