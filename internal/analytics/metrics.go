package analytics

// CacheHitRate is the share of the prompt read from the cache: cache reads
// over new input plus cache reads plus cache writes. Output is not part of
// the prompt. ok is false when there was no prompt to measure.
func CacheHitRate(input, read, write float64) (rate float64, ok bool) {
	prompt := input + read + write
	if prompt <= 0 {
		return 0, false
	}
	return read / prompt, true
}

// SpeedResult says whether a request's timing tells an output speed.
type SpeedResult int

const (
	SpeedMeasured SpeedResult = iota
	// SpeedNoTiming: the history records no duration at all.
	SpeedNoTiming
	// SpeedNoTTFT: a duration but no time to first token, so waiting and
	// writing cannot be told apart.
	SpeedNoTTFT
	// SpeedBurst: the reply arrived in one piece (or wrote nothing), so its
	// generation window says nothing about speed.
	SpeedBurst
)

// Bounds that tell a burst from a stream, as Magpie's MinDecodeMs,
// MaxDecodeSpeed, HeldWindowMs and HeldShare do.
const (
	minDecodeMs    = 100
	maxDecodeSpeed = 10000
	heldWindowMs   = 1000
	heldShare      = 4
	minFloorTokens = 1000
	minFloorMs     = 1000
)

// Decode returns the generation window in ms of a request that wrote out
// tokens, with ttft ms to its first token and total ms in all, and whether
// that window measures a speed. Speed is out*1000/window; a session's speed
// sums the outputs and windows of its measured requests.
func Decode(out, ttft, total float64) (window float64, r SpeedResult) {
	switch {
	case ttft <= 0 && total <= 0:
		return 0, SpeedNoTiming
	case ttft <= 0:
		return 0, SpeedNoTTFT
	}
	w := total - ttft
	if out <= 0 || w < minDecodeMs || out*1000 > maxDecodeSpeed*w || (w < heldWindowMs && ttft > heldShare*w) {
		return 0, SpeedBurst
	}
	return w, SpeedMeasured
}

// Floor returns a lower-bound generation window in ms when a request recorded
// total duration (including wait for the first token) without a usable TTFT.
// Below minFloorTokens or minFloorMs the wait dominates too much to be useful.
func Floor(out, total float64) (window float64, ok bool) {
	if out < minFloorTokens || total < minFloorMs || out*1000 > maxDecodeSpeed*total {
		return 0, false
	}
	return total, true
}
