//go:build !linux

package main

type noMeter struct{}

func (noMeter) sample() loadSample { return loadSample{} }

func newLoadMeter() loadMeter { return noMeter{} }
