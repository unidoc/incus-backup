//go:build race

package demo

func init() { raceEnabled = true } // the race detector slows the seed several times
