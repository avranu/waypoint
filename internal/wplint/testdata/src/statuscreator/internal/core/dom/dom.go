// Package dom is a domain under internal/core.
package dom

type CommitCheck struct{ Name, State, Creator string }

func DecodeBad() int {
	var list []struct { // want `without who posted them`
		Context string `json:"context"`
		State   string `json:"state"`
	}
	return len(list)
}

func DecodeGood() int {
	var list []struct {
		Context string `json:"context"`
		State   string `json:"state"`
		Creator string `json:"creator"`
	}
	return len(list)
}

func ByName(c CommitCheck) bool {
	return c.Name == "waypoint/ci" // want `waypoint/ci is compared by name alone`
}
