package store

// ProgressFunc receives a long operation's advancement: which phase is running,
// how many units it has finished, and how many there are in total. It exists so
// an interactive command can show that a redaction of a large journal — which
// walks and rebuilds every commit from the earliest affected one to the tip — is
// moving rather than hung. store deliberately reports numbers only; how (or
// whether) they are drawn is the command's business.
//
// Implementations must be cheap and must not block: they are called from inside
// the rewrite loop, which holds the journal lock.
type ProgressFunc func(phase string, done, total int)

// The phases a redaction reports. Each is a full pass over the journal, and on a
// large one each takes long enough to need its own indicator.
const (
	PhaseLocate  = "locating flagged blobs" // resolving every (commit, path) pair
	PhaseRead    = "reading flagged blobs"  // reading each distinct blob once
	PhaseRewrite = "rewriting journal"      // rebuilding and re-parenting commits
)

// report forwards one advancement to the configured ProgressFunc, if any.
//
// EVERY unit is reported — the reporting must not be throttled here, only in
// whatever draws it. Throttling by count (one call per N units) is wrong exactly
// where an indicator matters most: these phases cost wildly different amounts
// per unit, so "every 256th" meant a redraw every few milliseconds while a
// redaction skipped its untouched prefix and one every seventeen SECONDS while
// it rebuilt the affected tail — a display that froze precisely when the work
// got slow. The calls are cheap next to the git work they sit beside (one per
// journal commit, against several process spawns), and a time-based throttle in
// the consumer bounds the drawing regardless.
func (r *Recorder) report(phase string, done, total int) {
	if r.Progress == nil {
		return
	}
	r.Progress(phase, done, total)
}
