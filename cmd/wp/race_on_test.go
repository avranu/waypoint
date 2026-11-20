//go:build race

package main

// raceEnabled: the race detector slows code 5-10x, so timing checks skip.
const raceEnabled = true
