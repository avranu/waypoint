package numbering

import (
	"reflect"
	"strconv"
	"testing"
)

const mig = "internal/db/migrations/"

func mf(names ...string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = mig + n
	}
	return out
}

func rpr(id int64, number, place int, adds ...string) PR {
	return PR{ID: id, Number: number, Place: place, Adds: mf(adds...)}
}

// moveStrings is each move as "PR:from>to".
func moveStrings(ms []Move) []string {
	var out []string
	for _, m := range ms {
		out = append(out, strconv.FormatInt(m.PR, 10)+":"+m.From[len(mig):]+">"+m.To[len(mig):])
	}
	return out
}

func TestPlanRenumber(t *testing.T) {
	base := mf("0001_a.sql", "0002_b.sql", "0003_c.sql")
	for _, tc := range []struct {
		name string
		base []string
		prs  []PR
		want []string
	}{
		{"nothing clashes", base, []PR{rpr(1, 10, -1, "0004_x.sql"), rpr(2, 11, -1, "0005_y.sql")}, nil},
		{"two open pull requests share a number: the later one moves", base,
			[]PR{rpr(1, 10, -1, "0004_x.sql"), rpr(2, 11, -1, "0004_y.sql")},
			[]string{"2:0004_y.sql>0005_y.sql"}},
		{"overtaken by the base: it moves above everything open, nobody else moves", mf("0001_a.sql", "0002_b.sql", "0003_c.sql", "0004_d.sql"),
			[]PR{rpr(1, 10, -1, "0004_x.sql"), rpr(2, 11, -1, "0007_y.sql")},
			[]string{"1:0004_x.sql>0008_x.sql"}},
		{"line member lower than one ahead of it", base,
			[]PR{rpr(1, 10, 1, "0004_x.sql"), rpr(2, 11, 0, "0006_y.sql")},
			[]string{"1:0004_x.sql>0007_x.sql"}},
		{"a pull request outside the line may be lower than a line member", base,
			[]PR{rpr(1, 10, 0, "0006_x.sql"), rpr(2, 11, -1, "0004_y.sql")}, nil},
		{"the line goes before the rest whatever the numbers", base,
			[]PR{rpr(1, 10, -1, "0004_x.sql"), rpr(2, 11, 0, "0004_y.sql")},
			[]string{"1:0004_x.sql>0005_x.sql"}},
		{"two files, only the first clashes: both move in order", base,
			[]PR{rpr(1, 10, -1, "0003_x.sql", "0004_y.sql")},
			[]string{"1:0003_x.sql>0005_x.sql", "1:0004_y.sql>0006_y.sql"}},
		{"two files, only the second clashes: only it and what follows move", base,
			[]PR{rpr(1, 10, -1, "0005_q.sql"), rpr(2, 11, -1, "0004_x.sql", "0005_y.sql")},
			[]string{"2:0005_y.sql>0006_y.sql"}},
		{"names not starting with digits are ignored", base,
			[]PR{rpr(1, 10, -1, "README.md", "x1_a.sql", "0004_x.sql"), rpr(2, 11, -1, "notes.sql", "0004_y.sql")},
			[]string{"2:0004_y.sql>0005_y.sql"}},
		{"two files with one number in one pull request are left alone", base,
			[]PR{rpr(1, 10, -1, "0004_x.sql", "0004_y.sql")}, nil},
		{"two files with one number break another clause: both move", base,
			[]PR{rpr(1, 10, -1, "0002_x.sql", "0002_y.sql")},
			[]string{"1:0002_x.sql>0004_x.sql", "1:0002_y.sql>0005_y.sql"}},
		{"width is kept", mf("0189_a.sql", "0190_b.sql"),
			[]PR{rpr(1, 10, -1, "0189_x.sql")}, []string{"1:0189_x.sql>0191_x.sql"}},
		{"a number that outgrows its width is not renamed", mf("98_a.sql", "99_b.sql"),
			[]PR{rpr(1, 10, -1, "98_x.sql", "99_y.sql")}, nil},
		{"a move never lands on a number a pull request outside the line uses", base,
			[]PR{rpr(1, 10, 0, "0002_x.sql"), rpr(2, 11, -1, "0004_y.sql")},
			[]string{"1:0002_x.sql>0005_x.sql"}},
		{"a line member moved above another one moves that one too", base,
			[]PR{rpr(1, 10, 0, "0002_x.sql"), rpr(2, 11, 1, "0004_y.sql")},
			[]string{"1:0002_x.sql>0005_x.sql", "2:0004_y.sql>0006_y.sql"}},
		{"line members by place, not number", base,
			[]PR{rpr(3, 5, 0, "0004_a.sql"), rpr(1, 10, 1, "0004_b.sql"), rpr(2, 11, -1, "0004_c.sql")},
			[]string{"1:0004_b.sql>0005_b.sql", "2:0004_c.sql>0006_c.sql"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			moves, _ := Plan(tc.base, tc.prs)
			got := moveStrings(moves)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			// Applying the renames and planning again renames nothing.
			next := make([]PR, len(tc.prs))
			for i, pr := range tc.prs {
				next[i] = pr
				next[i].Adds = append([]string(nil), pr.Adds...)
			}
			for _, m := range moves {
				for i := range next {
					if next[i].ID != m.PR {
						continue
					}
					for j, a := range next[i].Adds {
						if a == m.From {
							next[i].Adds[j] = m.To
						}
					}
				}
			}
			if again, _ := Plan(tc.base, next); len(again) != 0 {
				t.Errorf("second pass renames %q", moveStrings(again))
			}
		})
	}
}

func TestPlanRenumberSaysWhy(t *testing.T) {
	base := mf("0001_a.sql", "0002_b.sql")
	ms, _ := Plan(base, []PR{rpr(1, 10, -1, "0001_x.sql", "0009_z.sql"), rpr(2, 11, -1, "0003_y.sql"), rpr(3, 12, -1, "0003_w.sql")})
	if len(ms) != 3 || ms[0].Why != WhyBase || ms[1].Why != WhyAfter || ms[2].Why != WhyPR || ms[2].By != 11 {
		t.Fatalf("moves = %+v", ms)
	}
}

// Why one source of order matters: two callers that order the same pull
// requests differently each move the other's file in turn.
func TestPlanOrdersDisagree(t *testing.T) {
	base := mf("0001_a.sql")
	f, a := rpr(1, 10, -1, "0002_f.sql"), rpr(2, 11, -1, "0002_a.sql")
	f.Place, a.Place = 0, 1
	first, _ := Plan(base, []PR{f, a})
	f.Place, a.Place = 1, 0
	second, _ := Plan(base, []PR{f, a})
	if got := moveStrings(first); !reflect.DeepEqual(got, []string{"2:0002_a.sql>0003_a.sql"}) {
		t.Fatalf("order F,A: %q", got)
	}
	if got := moveStrings(second); !reflect.DeepEqual(got, []string{"1:0002_f.sql>0003_f.sql"}) {
		t.Fatalf("order A,F: %q", got)
	}
}

func TestPlanReportsFilesThatDoNotFit(t *testing.T) {
	base := mf("98_a.sql", "99_b.sql")
	moves, stuck := Plan(base, []PR{rpr(1, 10, -1, "98_x.sql", "99_y.sql"), rpr(2, 11, -1, "0100_z.sql")})
	if len(moves) != 0 || !reflect.DeepEqual(stuck, mf("98_x.sql", "99_y.sql")) {
		t.Fatalf("moves %q, stuck %q", moveStrings(moves), stuck)
	}
}

func TestFileNumber(t *testing.T) {
	for p, want := range map[string]int{mig + "0189_x.sql": 189, "0007.sql": 7, mig + "x1.sql": -1, mig + "_1.sql": -1, mig + "12345678901_x.sql": -1} {
		if n, _, ok := FileNumber(p); (!ok && want != -1) || (ok && n != want) {
			t.Errorf("%s: %d %v", p, n, ok)
		}
	}
	if got, ok := withNumber(mig+"0189_note.sql", 191); got != mig+"0191_note.sql" || !ok {
		t.Errorf("withNumber = %s", got)
	}
	if _, ok := withNumber(mig+"98_note.sql", 100); ok {
		t.Errorf("withNumber made a number wider")
	}
}
