package core

type CommitCheck struct{ Name, State, Creator string }

func DecodeBad() int {
	var list []struct { // want `without who posted them`
		Context string `json:"context"`
		State   string `json:"state"`
	}
	return len(list)
}

type statusPost struct {
	Context string `json:"context"`
	State   string `json:"state"`
}

func DecodeGood() int {
	var list []struct {
		Context string   `json:"context"`
		State   string   `json:"state"`
		Creator struct{} `json:"creator"`
	}
	return len(list)
}

func GateBad(cs []CommitCheck) bool {
	for _, c := range cs {
		if c.Name == "waypoint/review" { // want `waypoint/review is compared by name alone`
			return true
		}
	}
	return false
}

func GateGood(cs []CommitCheck, self string) bool {
	for _, c := range cs {
		if c.Name == "waypoint/review" && c.Creator == self {
			return true
		}
	}
	return false
}

func CI(cs []CommitCheck) bool {
	for _, c := range cs {
		if c.Name == "ci / lint" {
			return true
		}
	}
	return false
}
