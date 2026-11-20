package main

import "testing"

func TestReadValueNonASCIILineDoesNotPanic(t *testing.T) {
	// "İ" lower-cases to a shorter string; offsets must still line up.
	got := readValue("İİİİİİ 12 ms\n\xff\xff\xff 5 ms", "ms")
	if got == nil || *got != 5 {
		t.Fatalf("got %v, want 5", got)
	}
	got = readValue("İİİİİİ 12 MS", "ms")
	if got == nil || *got != 12 {
		t.Fatalf("got %v, want 12", got)
	}
}
