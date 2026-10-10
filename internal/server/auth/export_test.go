package auth

import "time"

// TOTPForTest computes the code an authenticator app would show.
func TOTPForTest(b32secret string, at time.Time) string {
	s, _ := b32.DecodeString(b32secret)
	return totpCode(s, at.Unix()/totpPeriod)
}

// ArgonPeakForTest returns and resets the highest number of concurrent Argon2id computations seen.
func ArgonPeakForTest() int64 { return argonPeak.Swap(0) }

// ArgonSlotsForTest is the concurrency bound.
func ArgonSlotsForTest() int { return cap(argonSlots) }
