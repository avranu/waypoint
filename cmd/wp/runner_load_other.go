//go:build !linux

package main

// Only Linux says how busy it is (WP-366): elsewhere the runner takes up to
// --max-jobs whatever the load.

type noMeter struct{}

func (noMeter) sample() loadSample { return loadSample{} }

func newLoadMeter() loadMeter { return noMeter{} }
