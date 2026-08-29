package tui

// maxInputHeight bounds how much of the screen the composer may claim, so a
// long paste cannot squeeze the transcript out of existence.
const maxInputHeight = 8

// Layout is the row budget for one frame.
//
// It is deliberately plain arithmetic over integers with no reference to the
// renderer: the whole point is that the sizing rules can be tested without a
// terminal, and that View does nothing but fill the rows this hands it.
type Layout struct {
	Width  int
	Height int
	// Header is the row count for the identity/status bar.
	Header int
	// Rules counts horizontal separators (at most two: below the header and
	// above the composer). They are the first thing sacrificed when the
	// terminal is too short.
	Rules int
	// Body is the transcript viewport height.
	Body int
	// Approval is the height of the inline approval prompt, zero when none is
	// pending. It is capped at half the screen so the conversation that led to
	// the request stays visible (§49).
	Approval int
	// Input is the composer height.
	Input int
	// Footer is the status/hints row.
	Footer int
}

// Rows returns the total rows the layout occupies, which always equals Height
// for any usable terminal size.
func (l Layout) Rows() int {
	return l.Header + l.Rules + l.Body + l.Approval + l.Input + l.Footer
}

// ContentWidth is the usable text width inside the horizontal padding.
func (l Layout) ContentWidth() int {
	return maxInt(1, l.Width-2*framePadding)
}

// framePadding is the left/right gutter applied to every region.
const framePadding = 1

// ComputeLayout divides height between the regions.
//
// Shrinking order matters more than the ideal sizes: as the window gets
// shorter Boop gives up composer rows, then the approval detail, then the
// separators, then the footer, then the header — the transcript is the last
// thing to go, because a UI that cannot show what happened is not worth
// drawing. The result always satisfies Rows() == Height when height >= 1.
func ComputeLayout(width, height, inputLines, approvalLines int) Layout {
	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}
	l := Layout{
		Width:    width,
		Height:   height,
		Header:   1,
		Rules:    2,
		Input:    clampInt(inputLines, 1, maxInputHeight),
		Approval: clampInt(approvalLines, 0, height/2),
		Footer:   1,
	}

	// Give up regions, cheapest first, until the transcript fits.
	for l.body(height) < 1 {
		switch {
		case l.Input > 1:
			l.Input--
		case l.Approval > 1:
			l.Approval--
		case l.Rules > 0:
			l.Rules--
		case l.Footer > 0:
			l.Footer--
		case l.Header > 0:
			l.Header--
		case l.Approval > 0:
			l.Approval--
		case l.Input > 0:
			l.Input--
		default:
			// One row left and nothing else to reclaim.
			l.Body = 1
			return l
		}
	}
	l.Body = l.body(height)
	return l
}

// body is the rows left over for the transcript given the current budget.
func (l Layout) body(height int) int {
	return height - (l.Header + l.Rules + l.Approval + l.Input + l.Footer)
}

// InputLines reports how many rows a composer holding text of the given
// logical line count needs, clamped to the allowed range.
func InputLines(logical int) int { return clampInt(logical, 1, maxInputHeight) }

// HeaderSegments splits the header width between the identity field on the
// left and the status field on the right (§19: small identity left, larger
// provider/model/status area to the right).
//
// The identity field never grows past a third of the bar, and never shrinks
// below what "BOOP" needs, so the status side keeps the space it needs to say
// something useful.
func HeaderSegments(width int) (left, right int) {
	if width <= 0 {
		return 0, 0
	}
	const minIdentity = 4
	left = clampInt(width/3, 0, 18)
	if left < minIdentity {
		left = minInt(minIdentity, width)
	}
	right = width - left
	if right < 0 {
		right = 0
	}
	return left, right
}
