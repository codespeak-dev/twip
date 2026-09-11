package main

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codespeak-dev/twip/internal/store"
)

// TestProgressNonTerminalStaysQuiet: the indicator animates only on a terminal.
// A redirected run (a hook, CI, `twip redact > log`) must not get carriage
// returns and erase padding written into its log — it gets an occasional plain
// heartbeat line instead, throttled so a long phase does not flood the file.
func TestProgressNonTerminalStaysQuiet(t *testing.T) {
	var buf bytes.Buffer
	p := newProgress(&buf)
	defer p.close()
	if p.tty {
		t.Fatal("a bytes.Buffer was taken for a terminal")
	}
	for i := 1; i <= 5000; i++ {
		p.step(store.PhaseRewrite, i, 5000)
	}
	p.done()

	out := buf.String()
	if strings.ContainsAny(out, "\r") {
		t.Errorf("carriage returns written to a non-terminal:\n%q", out)
	}
	if n := strings.Count(out, "\n"); n > 2 {
		t.Errorf("heartbeat printed %d lines for one phase, want at most 2:\n%s", n, out)
	}
	if !strings.Contains(out, store.PhaseRewrite) {
		t.Errorf("heartbeat does not name the phase:\n%q", out)
	}
}

// TestProgressAnimatesWhileWorkStalls is the regression this indicator exists
// for: a phase whose units take a long time each must keep animating between
// reports. The first version redrew only when the work reported, and the work
// reported only every 256th unit, so the display froze for ~17 seconds at a time
// during the slowest stretch of a real redaction — indistinguishable from a hang.
func TestProgressAnimatesWhileWorkStalls(t *testing.T) {
	var buf syncBuffer
	p := &progress{w: &buf, tty: true, start: time.Now(), quit: make(chan struct{})}
	p.wg.Add(1)
	go p.animate()
	defer p.close()

	p.step(store.PhaseRewrite, 1, 5000) // one report, then the "work" stalls
	time.Sleep(6 * redrawEvery)

	frames := map[rune]bool{}
	for _, r := range buf.String() {
		for _, f := range spinnerFrames {
			if r == f {
				frames[r] = true
			}
		}
	}
	if len(frames) < 2 {
		t.Errorf("spinner drew %d distinct frames while the work reported nothing; want it to keep moving", len(frames))
	}
}

// TestProgressRateIgnoresAnEarlierRegime: a redaction's rewrite phase skips its
// untouched prefix at hundreds of thousands of commits a second and then rebuilds
// the tail at tens. A phase-average rate reports the prefix's speed forever —
// "eta 1s" printed over several minutes of work — so the window has to forget it.
func TestProgressRateIgnoresAnEarlierRegime(t *testing.T) {
	p := &progress{w: &bytes.Buffer{}, tty: true, quit: make(chan struct{})}
	p.begin(store.PhaseRewrite, true)

	// A burst long ago, then a slow crawl inside the window.
	now := time.Now()
	p.samples = []sample{
		{now.Add(-rateWindow - 10*time.Second), 0}, // aged out
		{now.Add(-10 * time.Second), 500000},       // window base
	}
	p.cur, p.total = 500100, 505000
	p.observe(p.cur) // trims what has aged out

	if got := p.rate(); got < 1 || got > 100 {
		t.Errorf("rate = %.0f/s, want the recent ~10/s crawl and not the earlier burst", got)
	}
	if eta := p.bar(); !strings.Contains(eta, "eta") || strings.Contains(eta, "eta 0s") {
		t.Errorf("bar = %q, want a real ETA for 4,900 units at the crawl rate", eta)
	}
}

// TestProgressBarReachesItsTotal: a bar that stops short of 100% reads as a hang,
// which is the exact thing it exists to rule out.
func TestProgressBarReachesItsTotal(t *testing.T) {
	p := &progress{w: &bytes.Buffer{}, tty: true, start: time.Now(), quit: make(chan struct{})}
	p.cur, p.total = 5000, 5000
	if got := p.bar(); !strings.Contains(got, "100%") || !strings.Contains(got, "5,000/5,000") {
		t.Errorf("bar at completion = %q, want 100%% and 5,000/5,000", got)
	}
	p.cur, p.total = 0, 0
	if got := p.bar(); strings.Contains(got, "NaN") || strings.Contains(got, "+Inf") {
		t.Errorf("bar with a zero total = %q, want no division artifacts", got)
	}
	// The line has to fit a conventional terminal, or a carriage return cannot
	// erase it and the bar smears down the screen.
	p.label, p.cur, p.total = store.PhaseRewrite, 221430, 527126
	p.samples = []sample{{time.Now().Add(-10 * time.Second), 209000}}
	if n := len([]rune(p.label)) + len([]rune(p.bar())) + 2; n > 72 {
		t.Errorf("progress line is %d columns wide, want <= 72", n)
	}
}

func TestCommasAndDuration(t *testing.T) {
	for in, want := range map[int]string{0: "0", 7: "7", 999: "999", 1000: "1,000",
		527126: "527,126", -1234: "-1,234"} {
		if got := commas(in); got != want {
			t.Errorf("commas(%d) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[time.Duration]string{
		9 * time.Second: "9s", 90 * time.Second: "1m30s", 3800 * time.Second: "1h03m"} {
		if got := humanDuration(in); got != want {
			t.Errorf("humanDuration(%s) = %q, want %q", in, got, want)
		}
	}
}

// syncBuffer is a bytes.Buffer the animation goroutine and the test can both
// touch without racing.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
