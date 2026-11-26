package kernel

import (
	"testing"
	"time"
)

func TestValueHelpers(t *testing.T) {
	s := "x"
	n := int64(4)
	for _, tc := range []struct {
		name      string
		got, want any
	}{
		{"FirstNonEmpty a", FirstNonEmpty("a", "b"), "a"},
		{"FirstNonEmpty b", FirstNonEmpty("", "b"), "b"},
		{"TrimDeref nil", TrimDeref(nil), ""},
		{"TrimDeref trims", TrimDeref(&[]string{" x "}[0]), "x"},
		{"Ptr", *Ptr(s), "x"},
		{"NullInt nil", NullInt(nil), nil},
		{"NullInt set", NullInt(&n), int64(4)},
		{"BoolInt true", BoolInt(true), 1},
		{"BoolInt false", BoolInt(false), 0},
		{"MillisToTime", MillisToTime(1500), time.Unix(1, 5e8).UTC()},
		{"Itoa64", Itoa64(-12), "-12"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

func TestSHAAndCountHelpers(t *testing.T) {
	for _, tc := range []struct {
		name      string
		got, want any
	}{
		{"ShortSHA long", ShortSHA("0123456789"), "0123456"},
		{"ShortSHA short", ShortSHA("012"), "012"},
		{"SameSHA prefix", SameSHA("ABCdef123", "abcd"), true},
		{"SameSHA other way", SameSHA("abcd", "abcdef"), true},
		{"SameSHA differ", SameSHA("abc", "abd"), false},
		{"SameSHA empty", SameSHA("", ""), false},
		{"PluralS one", PluralS(1), ""},
		{"PluralS many", PluralS(2), "s"},
		{"PluralS zero", PluralS(0), "s"},
		{"CountNoun one", CountNoun(1, "run"), "1 run"},
		{"CountNoun many", CountNoun(3, "run"), "3 runs"},
		{"TruncateBytes fits", TruncateBytes("abc", 3), "abc"},
		{"TruncateBytes cuts", TruncateBytes("abcdefgh", 6), "abc…"},
		{"TruncateBytes mid rune", TruncateBytes("aéééé", 6), "aé…"},
		{"TruncateBytes only the ellipsis", TruncateBytes("hello", 3), "…"},
		{"TruncateBytes under the ellipsis", TruncateBytes("hello", 2), ""},
		{"TruncateBytes one", TruncateBytes("hello", 1), ""},
		{"TruncateBytes zero", TruncateBytes("hello", 0), ""},
		{"TruncateBytes negative", TruncateBytes("hello", -1), ""},
		{"TruncateBytes short enough", TruncateBytes("hi", 2), "hi"},
		{"ClipRunes fits", ClipRunes("abc", 3), "abc"},
		{"ClipRunes cuts", ClipRunes("abcdef", 4), "abc…"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}
