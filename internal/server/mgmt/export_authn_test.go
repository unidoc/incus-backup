package mgmt

import "time"

// ResetThrottleForTest forgets the per-address sign-in counts (tests sign in more than 20 times a
// minute from 127.0.0.1).
func (s *Server) ResetThrottleForTest() {
	s.init()
	s.throttle = newIPLimiter(20, time.Minute)
}
