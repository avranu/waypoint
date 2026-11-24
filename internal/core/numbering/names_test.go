package numbering

import "testing"

func TestReplaceNames(t *testing.T) {
	for _, tc := range []struct {
		name  string
		moves [][2]string
		text  string
		want  string
	}{
		{"a longer name sharing the stem is left alone", [][2]string{{"a/0003_add_user.sql", "a/0004_add_user.sql"}},
			"0003_add_user 0003_add_user_roles 10003_add_user", "0004_add_user 0003_add_user_roles 10003_add_user"},
		{"a path and a quoted name", [][2]string{{"m/0003_add_user.sql", "m/0007_add_user.sql"}},
			`"migrations/0003_add_user.sql"`, `"migrations/0007_add_user.sql"`},
		{"a down file", [][2]string{{"m/0003_add_user.sql", "m/0007_add_user.sql"}},
			"0003_add_user.down.sql", "0007_add_user.down.sql"},
		{"a bare number is matched with its extension", [][2]string{{"m/0002.sql", "m/0005.sql"}},
			"0002.sql 0002 10002.sql 0002.sqlx", "0005.sql 0002 10002.sql 0002.sqlx"},
		{"one stem prefixes the other: each is replaced whole", [][2]string{{"m/0003_a.sql", "m/0007_a.sql"}, {"m/0003_a_b.sql", "m/0008_a_b.sql"}},
			"0003_a, 0003_a_b and 0003_a-b", "0007_a, 0008_a_b and 0007_a-b"},
		{"the replacement is not scanned again", [][2]string{{"m/0001_x.sql", "m/0002_x.sql"}, {"m/0002_x.sql", "m/0003_x.sql"}},
			"0001_x 0002_x", "0002_x 0003_x"},
		{"nothing to replace", nil, "0001_x", "0001_x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rs []Rename
			for _, m := range tc.moves {
				rs = append(rs, NameChange(m[0], m[1]))
			}
			if got := ReplaceNames(tc.text, rs); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMentions(t *testing.T) {
	r := NameChange("m/0003_add_user.sql", "m/0007_add_user.sql")
	for text, want := range map[string]bool{
		"+x 0003_add_user": true, "+0003_add_user_roles": false, "+10003_add_user": false, "+\"0003_add_user.sql\"": true, "no": false,
	} {
		if got := Mentions(text, r.From); got != want {
			t.Errorf("Mentions(%q) = %v", text, got)
		}
	}
}
