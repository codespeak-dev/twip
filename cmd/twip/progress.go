package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// progress draws a single-line activity indicator for the long phases of
// `twip redact`. On a real journal a redaction scans and rewrites hundreds of
// thousands of commits, which would otherwise print nothing at all between
// "Scanning …" and the result — minutes or hours during which the only honest
// reading is "it might be frozen".
//
// Two properties matter more than the drawing:
//
//   - The animation runs on its OWN clock, not on the work's. A phase that
//     reports once per rebuilt commit, where a commit takes half a second,
//     would otherwise tick twice a second; one blocked on a single slow step
//     would look hung — the exact reading the indicator exists to rule out.
//   - The rate and ETA come from a recent window, not from the phase average.
//     A redaction's rewrite phase runs at two speeds (it skips an untouched
//     prefix at hundreds of thousands of commits a second, then rebuilds the
//     affected tail at tens), and an average over both predicts "eta 1s" for
//     work that takes minutes.
//
// It draws to STDERR: the command's own report goes to stdout, so a piped or
// captured run keeps exactly the output it had before.
type progress struct {
	mu          sync.Mutex
	w           io.Writer
	tty         bool      // animate, i.e. redraw one line in place
	label       string    // current phase; "" when idle
	determinate bool      // a bar (size known) rather than a bare spinner
	cur, total  int       // latest counters for the current phase
	start       time.Time // when the current phase began
	frame       int       // spinner position
	lastLen     int       // width of the line to erase on the next draw
	lastBeat    time.Time // non-terminal heartbeat clock
	samples     []sample  // recent (time, count) pairs behind the rate and ETA

	quit chan struct{}
	once sync.Once
	wg   sync.WaitGroup
}

// sample is one observation of a phase's progress, for the rate window.
type sample struct {
	t    time.Time
	done int
}

const (
	// redrawEvery is the animation cadence — fast enough to read as motion,
	// slow enough that the terminal is not the bottleneck.
	redrawEvery = 100 * time.Millisecond
	// heartbeatEvery is the non-terminal cadence: a redirected run (CI, a hook,
	// `twip redact > log`) gets an occasional plain line instead of an
	// animation, often enough to prove liveness and rarely enough to keep the
	// log readable.
	heartbeatEvery = 30 * time.Second
	// rateWindow is how far back the rate and ETA look, and sampleEvery how
	// often a new observation enters that window.
	rateWindow  = 20 * time.Second
	sampleEvery = time.Second
)

var spinnerFrames = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

// newProgress returns a reporter drawing to w. Animation is enabled only when w
// is a terminal — writing carriage returns and erase padding into a file or a
// pipe would corrupt it. An os.File is the only writer twip passes here; a
// test's buffer is treated as "not a terminal", which is what makes the output
// assertable. Call close when done, to stop the animation goroutine.
func newProgress(w io.Writer) *progress {
	f, ok := w.(*os.File)
	p := &progress{w: w, tty: ok && isTerminal(f), start: time.Now(), quit: make(chan struct{})}
	if p.tty {
		p.wg.Add(1)
		go p.animate()
	}
	return p
}

// animate redraws the active phase on a fixed cadence for as long as there is
// one. It is what decouples the indicator's liveness from the work's reporting.
func (p *progress) animate() {
	defer p.wg.Done()
	t := time.NewTicker(redrawEvery)
	defer t.Stop()
	for {
		select {
		case <-p.quit:
			return
		case <-t.C:
			p.mu.Lock()
			if p.label != "" {
				p.frame++
				p.render()
			}
			p.mu.Unlock()
		}
	}
}

// close ends the animation and clears the line. Safe to call more than once.
func (p *progress) close() {
	p.once.Do(func() {
		close(p.quit)
		p.wg.Wait()
	})
	p.done()
}

// step reports a determinate phase's advancement. It only records the numbers —
// the drawing happens on the animation's clock — so callers may call it as often
// as they like, and should: reporting every unit is what lets the rate window
// see the work's real pace.
func (p *progress) step(label string, done, total int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if label != p.label {
		p.begin(label, true)
	}
	p.cur, p.total = done, total
	p.observe(done)
	if !p.tty {
		p.heartbeat(done, total)
	}
}

// spin starts an indeterminate phase — one whose end cannot be predicted because
// the work happens inside a scanner subprocess. The returned func ends it and
// must be called (defer it); it is safe to call more than once.
func (p *progress) spin(label string) (end func()) {
	p.mu.Lock()
	p.begin(label, false)
	p.mu.Unlock()
	var once sync.Once
	return func() { once.Do(p.done) }
}

// begin switches to a new phase, resetting its clock and rate window so the
// numbers describe this phase and not the one before it. Callers hold p.mu.
func (p *progress) begin(label string, determinate bool) {
	p.erase()
	p.label, p.determinate = label, determinate
	p.start, p.frame = time.Now(), 0
	p.cur, p.total = 0, 0
	p.samples, p.lastBeat = p.samples[:0], time.Time{}
}

// done clears the indicator, leaving the cursor at the start of a clean line so
// the command's own output prints normally.
func (p *progress) done() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.erase()
	p.label = ""
}

// observe records one point for the rate window, dropping what has aged out of
// it. Callers hold p.mu.
func (p *progress) observe(done int) {
	now := time.Now()
	if n := len(p.samples); n > 0 && now.Sub(p.samples[n-1].t) < sampleEvery {
		return
	}
	p.samples = append(p.samples, sample{now, done})
	for len(p.samples) > 1 && now.Sub(p.samples[0].t) > rateWindow {
		p.samples = p.samples[1:]
	}
}

// rate returns recent throughput in units per second, or 0 when the window is
// too young to say anything honest. Callers hold p.mu.
func (p *progress) rate() float64 {
	if len(p.samples) == 0 {
		return 0
	}
	base := p.samples[0]
	el := time.Since(base.t).Seconds()
	if el < 2 || p.cur <= base.done {
		return 0
	}
	return float64(p.cur-base.done) / el
}

// heartbeat prints the plain non-terminal progress line, throttled. Callers hold
// p.mu.
func (p *progress) heartbeat(done, total int) {
	if !p.lastBeat.IsZero() && time.Since(p.lastBeat) < heartbeatEvery && done != total {
		return
	}
	p.lastBeat = time.Now()
	fmt.Fprintf(p.w, "  %s: %s/%s (%d%%)\n", p.label, commas(done), commas(total), pct(done, total))
}

// render draws "<spinner> <label> <detail>" in place. Callers hold p.mu.
func (p *progress) render() {
	if !p.tty || p.label == "" {
		return
	}
	detail := humanDuration(time.Since(p.start))
	if p.determinate {
		detail = p.bar()
	}
	line := fmt.Sprintf("%s %s %s", string(spinnerFrames[p.frame%len(spinnerFrames)]), p.label, detail)
	pad := ""
	if n := p.lastLen - len([]rune(line)); n > 0 {
		pad = strings.Repeat(" ", n)
	}
	fmt.Fprintf(p.w, "\r%s%s", line, pad)
	p.lastLen = len([]rune(line))
}

// erase removes the drawn line. Callers hold p.mu.
func (p *progress) erase() {
	if !p.tty || p.lastLen == 0 {
		return
	}
	fmt.Fprintf(p.w, "\r%s\r", strings.Repeat(" ", p.lastLen))
	p.lastLen = 0
}

// barWidth is sized so the whole line — spinner, label, gauge, counts, rate and
// ETA — stays inside ~72 columns. A line that wraps cannot be erased by a
// carriage return, so it would leave a trail of half-drawn bars behind it.
const barWidth = 10

// bar renders the determinate detail: a gauge, the counts, and — once the rate
// window has something to say — the recent rate and the ETA it implies. Callers
// hold p.mu.
func (p *progress) bar() string {
	filled := 0
	if p.total > 0 {
		if filled = p.cur * barWidth / p.total; filled > barWidth {
			filled = barWidth
		}
	}
	gauge := strings.Repeat("█", filled) + strings.Repeat("·", barWidth-filled)
	detail := fmt.Sprintf("▕%s▏ %3d%% %s/%s", gauge, pct(p.cur, p.total), commas(p.cur), commas(p.total))
	if rate := p.rate(); rate > 0 {
		detail += " " + commas(int(rate)) + "/s"
		if p.total > p.cur {
			detail += " eta " + humanDuration(time.Duration(float64(p.total-p.cur)/rate)*time.Second)
		}
	}
	return detail
}

func pct(done, total int) int {
	if total <= 0 {
		return 0
	}
	return done * 100 / total
}

// humanDuration renders a coarse "4m12s" / "1h03m" — precision past the second
// is noise for a phase measured in minutes.
func humanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// commas groups an integer for readability: a journal's commit counts run to six
// digits, where "527126" and "52712" are hard to tell apart at a glance.
func commas(n int) string {
	if n < 0 {
		return "-" + commas(-n)
	}
	s := fmt.Sprintf("%d", n)
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}
