package wql

import (
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in    string
		terms []Term
		text  []string
		order []Order
	}{
		{in: "", terms: nil},
		{in: "login bug", text: []string{"login", "bug"}},
		{in: `"login page" crash`, text: []string{"login page", "crash"}},
		{
			in:    "assignee:me is:open",
			terms: []Term{{Key: "assignee", Op: "=", Values: []string{"me"}}, {Key: "is", Op: "=", Values: []string{"open"}}},
		},
		{
			in:    "type:bug,task -label:wontfix",
			terms: []Term{{Key: "type", Op: "=", Values: []string{"bug", "task"}}, {Key: "label", Op: "=", Values: []string{"wontfix"}, Negate: true}},
		},
		{
			in:    `status:"In Review" priority:>=high`,
			terms: []Term{{Key: "status", Op: "=", Values: []string{"In Review"}}, {Key: "priority", Op: ">=", Values: []string{"high"}}},
		},
		{
			in:    `Status:"a, b" due:<7d`,
			terms: []Term{{Key: "status", Op: "=", Values: []string{"a, b"}}, {Key: "due", Op: "<", Values: []string{"7d"}}},
		},
		{in: "order:-updated,priority", order: []Order{{Field: "updated", Desc: true}, {Field: "priority"}}},
		{in: "http://example.com", text: []string{"http://example.com"}},
		{in: "-foo", text: []string{"-foo"}},
		{in: `"unterminated phrase`, text: []string{"unterminated phrase"}},
		{in: "WAY-12", text: []string{"WAY-12"}},
	}
	for _, c := range cases {
		q, err := Parse(c.in)
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if !reflect.DeepEqual(q.Terms, c.terms) {
			t.Errorf("%q terms = %#v, want %#v", c.in, q.Terms, c.terms)
		}
		if !reflect.DeepEqual(q.Text, c.text) {
			t.Errorf("%q text = %#v, want %#v", c.in, q.Text, c.text)
		}
		if !reflect.DeepEqual(q.Order, c.order) {
			t.Errorf("%q order = %#v, want %#v", c.in, q.Order, c.order)
		}
	}
}

func TestParseErrors(t *testing.T) {
	if _, err := Parse("status:"); err == nil {
		t.Fatal("expected error for empty value")
	}
}

func TestQuoteRoundTrip(t *testing.T) {
	for _, v := range []string{"simple", "In Review", `with "quotes"`, "a,b", ""} {
		q, err := Parse("status:" + Quote(v))
		if v == "" {
			if err == nil {
				t.Errorf("empty value should not parse")
			}
			continue
		}
		if err != nil || len(q.Terms) != 1 || q.Terms[0].Values[0] != v {
			t.Errorf("round trip %q: %#v %v", v, q.Terms, err)
		}
	}
}

func TestQuotedLists(t *testing.T) {
	q, err := Parse(`status:"In Review",Done repo:"me/app","org/lib" label:a,"b c" -assignee:"x y"`)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"In Review", "Done"}, {"me/app", "org/lib"}, {"a", "b c"}, {"x y"}}
	if len(q.Terms) != len(want) || len(q.Text) != 0 {
		t.Fatalf("terms %+v text %v", q.Terms, q.Text)
	}
	for n, w := range want {
		if strings.Join(q.Terms[n].Values, "|") != strings.Join(w, "|") {
			t.Errorf("term %d values = %q, want %q", n, q.Terms[n].Values, w)
		}
	}
	if !q.Terms[3].Negate {
		t.Error("negation lost")
	}
	if _, err := Parse(`status:""`); err == nil {
		t.Error("empty quoted value accepted")
	}
}

func TestUnknownOperators(t *testing.T) {
	for _, in := range []string{"priority:<>high", "points:=>3", "due:!<today", "points:>>2"} {
		if _, err := Parse(in); err == nil {
			t.Errorf("%s parsed without an error", in)
		}
	}
	for _, in := range []string{"priority:>=high", "points:<3", "status:!=Done", "points:=3"} {
		if _, err := Parse(in); err != nil {
			t.Errorf("%s: %v", in, err)
		}
	}
}
