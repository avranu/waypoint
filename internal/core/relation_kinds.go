package core

import "strings"

// RelationKind is one kind of relation between two issues (WP-1036,
// WP-ADR105). Kinds live here, not in a CHECK on issue_relations, so a new
// one is a line in relationKindList.
type RelationKind struct {
	// Kind is what issue_relations stores: "A <kind> B", A the source.
	Kind string `json:"kind"`
	// Inverse is how B reads it ("blocked_by"); "" for a symmetric kind,
	// which reads the same from both ends and is stored once per pair.
	Inverse string `json:"inverse,omitempty"`
	// Subkinds narrow the kind; a relation may also have none.
	Subkinds []string `json:"subkinds,omitempty"`
}

// relationKindList are the kinds, in the order people pick them.
var relationKindList = []RelationKind{
	{Kind: "relates"},
	{Kind: "blocks", Inverse: "blocked_by"},
	{Kind: "duplicates", Inverse: "duplicated_by"},
	// A follows B: A is more work on what B did (WP-255). A fix mends
	// something B got wrong or missed; an expansion does more of it.
	{Kind: "follows", Inverse: "followed_by", Subkinds: []string{"fix", "expansion"}},
	// A supersedes B: A replaces B's plan or work, so work done for B
	// isn't a regression when A undoes it (WP-782).
	{Kind: "supersedes", Inverse: "superseded_by"},
	// The two pull against each other: doing one can undo or work against
	// the other, so whoever takes either should weigh both (WP-782).
	{Kind: "tension"},
}

// relationKindNames are the names a relation can be given, inverses too.
func relationKindNames() []string {
	var out []string
	for _, k := range relationKindList {
		out = append(out, k.Kind)
		if k.Inverse != "" {
			out = append(out, k.Inverse)
		}
	}
	return out
}

// storedRelation turns a kind as given (either end's name) into the stored
// kind, and whether the ends swap; ok is false for an unknown kind.
func storedRelation(kind string) (k *RelationKind, flip, ok bool) {
	for n := range relationKindList {
		switch kind {
		case relationKindList[n].Kind:
			return &relationKindList[n], false, true
		case relationKindList[n].Inverse:
			if kind != "" {
				return &relationKindList[n], true, true
			}
		}
	}
	return nil, false, false
}

// relationFrom is a stored kind as the target reads it.
func relationFrom(kind string, inverse bool) string {
	if !inverse {
		return kind
	}
	if k, _, ok := storedRelation(kind); ok && k.Kind == kind && k.Inverse != "" {
		return k.Inverse
	}
	return kind
}

// relationKindError says what a relation can be.
func relationKindError() error {
	return invalid("relation must be one of %s", strings.Join(relationKindNames(), ", "))
}
