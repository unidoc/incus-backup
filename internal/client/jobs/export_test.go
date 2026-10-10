package jobs

import "time"

// SetTimeouts overrides the per-type timeouts (tests).
func (x *Executor) SetTimeouts(fn func(typ string) time.Duration) { x.timeoutFn = fn }
