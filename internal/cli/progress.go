package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const (
	progressWidth    = 30
	progressInterval = 100 * time.Millisecond

	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
	clearLine  = "\x1b[K"
)

// progressBar draws the transfer of one file on one line:
//
//	[2/5] linux.tar.gz  ===========>                   37%  4.2/11.3 MiB  1.8 MiB/s
//
// On a terminal, the line is redrawn at most every progressInterval. Elsewhere (a CI, a
// redirection), only the final line is printed, so the log is not filled with \r.
type progressBar struct {
	w     io.Writer
	label string
	color bool
	live  bool

	start    time.Time
	last     time.Time
	done     int64
	total    int64
	finished bool

	now func() time.Time
}

func newProgressBar(w io.Writer, label string, color bool) *progressBar {
	b := &progressBar{w: w, label: label, color: color, live: isTerminal(w), total: -1, now: time.Now}
	b.start = b.now()
	return b
}

// isTerminal tells whether w is a character device, a terminal: no new dependency for that.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// Update records the progress; it is a util.ProgressFunc. A restart from 0 (a retry) restarts
// the speed too.
func (b *progressBar) Update(done, total int64) {
	if b.finished {
		return
	}
	now := b.now()
	if done == 0 {
		b.start = now
	}
	b.done, b.total = done, total
	if !b.live || (now.Sub(b.last) < progressInterval && done != total) {
		return
	}
	b.last = now
	_, _ = fmt.Fprint(b.w, "\r"+b.line(ansiCyan, "")+clearLine)
}

// Done draws the final line in green, with ✓.
func (b *progressBar) Done() {
	b.finish(ansiGreen, "✓")
}

// Skip draws the final line in yellow, with –: the server already has the version, and the
// command still succeeds (--if-exists skip).
func (b *progressBar) Skip() {
	b.finish(ansiYellow, "–")
}

// Fail draws the final line in red, with ✗.
func (b *progressBar) Fail() {
	b.finish(ansiRed, "✗")
}

func (b *progressBar) finish(color, mark string) {
	if b.finished {
		return
	}
	b.finished = true
	if b.live {
		_, _ = fmt.Fprint(b.w, "\r"+b.line(color, mark)+clearLine+"\n")
		return
	}
	size := humanBytes(b.done)
	if b.total >= 0 {
		size = humanBytes(b.total)
	}
	_, _ = fmt.Fprintf(b.w, "%s  %s  %s\n", b.label, size, b.paint(color, mark))
}

// line returns the progress line, with the bar in color and mark at the end. Without a known
// total, it shows the bytes and the speed only.
func (b *progressBar) line(color, mark string) string {
	var sb strings.Builder
	sb.WriteString(b.label)
	sb.WriteString("  ")
	if b.total > 0 {
		pct := b.done * 100 / b.total
		sb.WriteString(b.paint(color, bar(b.done, b.total)))
		sb.WriteString(b.paint(ansiBold, fmt.Sprintf(" %3d%%", pct)))
		_, _ = fmt.Fprintf(&sb, "  %s/%s", humanBytes(b.done), humanBytes(b.total))
	} else {
		sb.WriteString(humanBytes(b.done))
	}
	if elapsed := b.now().Sub(b.start).Seconds(); elapsed > 0 {
		_, _ = fmt.Fprintf(&sb, "  %s/s", humanBytes(int64(float64(b.done)/elapsed)))
	}
	if mark != "" {
		sb.WriteString("  ")
		sb.WriteString(b.paint(color, mark))
	}
	return sb.String()
}

func (b *progressBar) paint(color, s string) string {
	if !b.color || s == "" {
		return s
	}
	return color + s + ansiReset
}

// bar returns "=====>     " on progressWidth characters, all "=" once done reaches total.
func bar(done, total int64) string {
	filled := int(min(done, total) * progressWidth / total)
	if filled >= progressWidth {
		return strings.Repeat("=", progressWidth)
	}
	if filled == 0 {
		return ">" + strings.Repeat(" ", progressWidth-1)
	}
	return strings.Repeat("=", filled-1) + ">" + strings.Repeat(" ", progressWidth-filled)
}

// humanBytes returns n in B, KiB, MiB or GiB.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n) / unit
	for _, suffix := range []string{"KiB", "MiB", "GiB"} {
		if value < unit || suffix == "GiB" {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
		value /= unit
	}
	return ""
}
