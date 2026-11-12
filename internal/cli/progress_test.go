package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// liveBar returns a bar drawn as on a terminal, with a clock moved by hand.
func liveBar(color bool) (*progressBar, *bytes.Buffer, *time.Time) {
	var out bytes.Buffer
	now := time.Unix(0, 0)
	b := newProgressBar(&out, "[1/2] a.zip", color)
	b.live = true
	b.now = func() time.Time { return now }
	b.start = now
	return b, &out, &now
}

func TestBar(t *testing.T) {
	for _, tc := range []struct {
		done, total int64
		want        string
	}{
		{0, 100, ">" + strings.Repeat(" ", 29)},
		{37, 100, strings.Repeat("=", 10) + ">" + strings.Repeat(" ", 19)},
		{100, 100, strings.Repeat("=", 30)},
		{150, 100, strings.Repeat("=", 30)},
	} {
		if got := bar(tc.done, tc.total); got != tc.want {
			t.Errorf("bar(%d, %d) = %q, want %q", tc.done, tc.total, got, tc.want)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	for n, want := range map[int64]string{
		0:          "0 B",
		1023:       "1023 B",
		1024:       "1.0 KiB",
		11_848_909: "11.3 MiB",
		3 << 30:    "3.0 GiB",
		5 << 40:    "5120.0 GiB",
	} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestProgressLive(t *testing.T) {
	b, out, now := liveBar(false)
	b.Update(0, 1000)
	*now = now.Add(time.Second)
	b.Update(370, 1000)

	lines := strings.Split(out.String(), "\r")
	last := lines[len(lines)-1]
	want := "[1/2] a.zip  " + strings.Repeat("=", 10) + ">" + strings.Repeat(" ", 19) + "  37%  370 B/1000 B  370 B/s" + clearLine
	if last != want {
		t.Errorf("line = %q\nwant   %q", last, want)
	}

	b.Update(1000, 1000)
	b.Done()
	if !strings.HasSuffix(out.String(), strings.Repeat("=", 30)+" 100%  1000 B/1000 B  1000 B/s  ✓"+clearLine+"\n") {
		t.Errorf("final line = %q", out.String())
	}
}

func TestProgressThrottled(t *testing.T) {
	b, out, now := liveBar(false)
	b.Update(0, 1000)
	*now = now.Add(10 * time.Millisecond)
	b.Update(10, 1000)
	b.Update(20, 1000)
	if n := strings.Count(out.String(), "\r"); n != 1 {
		t.Errorf("%d redraws within %s, want 1", n, progressInterval)
	}
	b.Update(1000, 1000)
	if n := strings.Count(out.String(), "\r"); n != 2 {
		t.Errorf("the last update is not drawn: %d redraws", n)
	}
}

func TestProgressColors(t *testing.T) {
	b, out, _ := liveBar(true)
	b.Update(0, 10)
	if !strings.Contains(out.String(), ansiCyan+">") || !strings.Contains(out.String(), ansiBold+"   0%"+ansiReset) {
		t.Errorf("running line = %q", out.String())
	}
	b.Fail()
	if !strings.Contains(out.String(), ansiRed+"✗"+ansiReset) {
		t.Errorf("failed line = %q", out.String())
	}

	b, out, _ = liveBar(true)
	b.Update(10, 10)
	b.Done()
	if !strings.Contains(out.String(), ansiGreen+strings.Repeat("=", 30)+ansiReset) {
		t.Errorf("done line = %q", out.String())
	}
}

func TestProgressUnknownSize(t *testing.T) {
	b, out, now := liveBar(false)
	b.Update(0, -1)
	*now = now.Add(time.Second)
	b.Update(2048, -1)
	if !strings.HasSuffix(out.String(), "\r[1/2] a.zip  2.0 KiB  2.0 KiB/s"+clearLine) {
		t.Errorf("line = %q", out.String())
	}
}

func TestProgressNotLive(t *testing.T) {
	var out bytes.Buffer
	b := newProgressBar(&out, "[1/1] a.zip", true)
	b.Update(0, 2048)
	b.Update(2048, 2048)
	if out.Len() != 0 {
		t.Errorf("drawn off a terminal: %q", out.String())
	}
	b.Done()
	b.Fail()
	if got := out.String(); got != "[1/1] a.zip  2.0 KiB  "+ansiGreen+"✓"+ansiReset+"\n" {
		t.Errorf("final line = %q", got)
	}

	out.Reset()
	b = newProgressBar(&out, "[1/1] b.zip", false)
	b.Update(512, -1)
	b.Fail()
	if got := out.String(); got != "[1/1] b.zip  512 B  ✗\n" {
		t.Errorf("failed line = %q", got)
	}
}

func TestProgressRetryRestarts(t *testing.T) {
	b, _, now := liveBar(false)
	b.Update(0, 100)
	*now = now.Add(5 * time.Second)
	b.Update(50, 100)
	b.Update(0, 100)
	if !b.start.Equal(*now) || b.done != 0 {
		t.Errorf("a restart keeps start=%v done=%d", b.start, b.done)
	}
}

func TestIsTerminal(t *testing.T) {
	if isTerminal(&bytes.Buffer{}) {
		t.Error("a buffer is a terminal")
	}
}
