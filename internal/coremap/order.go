package coremap

import (
	"sort"
)

// suggestOrder returns an order of nodes, providers first, that minimises the
// weight of upward references: w[{a, b}] is the number of references from a to
// b, and one points up when b comes after a. A pinned node (if not "") stays
// first. It runs Eades-Lin-Smyth greedy on the sorted nodes, then moves single
// nodes to their best position until no move lowers the cost. The result
// depends only on the nodes and weights.
func suggestOrder(nodes []string, w map[[2]string]int, pinned string) []string {
	var free []string
	for _, n := range nodes {
		if n != pinned {
			free = append(free, n)
		}
	}
	sort.Strings(free)
	order := elsOrder(free, w)
	improve(order, w)
	if pinned != "" {
		order = append([]string{pinned}, order...)
	}
	return order
}

// elsOrder is Eades-Lin-Smyth on the graph with arcs provider -> consumer
// (the reverse of a reference), so a good order has arcs pointing forward.
func elsOrder(nodes []string, w map[[2]string]int) []string {
	rem := map[string]bool{}
	for _, n := range nodes {
		rem[n] = true
	}
	// Degrees are recomputed on demand: there are a few hundred groups at most.
	deg := func(n string) (o, i int) {
		for _, m := range nodes {
			if !rem[m] || m == n {
				continue
			}
			o += w[[2]string{m, n}] // m references n: arc n -> m
			i += w[[2]string{n, m}] // n references m: arc m -> n
		}
		return o, i
	}
	var left, right []string
	for len(rem) > 0 {
		moved := true
		for moved {
			moved = false
			for _, n := range nodes {
				if !rem[n] {
					continue
				}
				if o, _ := deg(n); o == 0 { // a sink: nothing left references it, so it goes last
					right = append(right, n)
					delete(rem, n)
					moved = true
				}
			}
			for _, n := range nodes {
				if !rem[n] {
					continue
				}
				if _, i := deg(n); i == 0 { // a source: it references nothing left, so it goes first
					left = append(left, n)
					delete(rem, n)
					moved = true
				}
			}
		}
		best, bd := "", 0
		for _, n := range nodes {
			if !rem[n] {
				continue
			}
			o, i := deg(n)
			if best == "" || o-i > bd {
				best, bd = n, o-i
			}
		}
		if best != "" {
			left = append(left, best)
			delete(rem, best)
		}
	}
	for i := len(right) - 1; i >= 0; i-- {
		left = append(left, right[i])
	}
	return left
}

// upCost is the weight of upward references under order.
func upCost(order []string, w map[[2]string]int) int {
	pos := map[string]int{}
	for i, n := range order {
		pos[n] = i
	}
	c := 0
	for k, x := range w {
		pa, ok1 := pos[k[0]]
		pb, ok2 := pos[k[1]]
		if ok1 && ok2 && pb > pa {
			c += x
		}
	}
	return c
}

// improve moves single nodes to their cheapest position until no move helps.
func improve(order []string, w map[[2]string]int) {
	for {
		changed := false
		for _, v := range append([]string(nil), order...) {
			i := indexOf(order, v)
			rest := append(append([]string(nil), order[:i]...), order[i+1:]...)
			// cost(k): v inserted before rest[k]. Referencing an earlier node is
			// free; an earlier node referencing v, or v referencing a later one, is upward.
			cost := 0
			for _, u := range rest {
				cost += w[[2]string{v, u}]
			}
			bestK, bestC := 0, cost
			curC := 0
			for k := 0; k <= len(rest); k++ {
				if k == i {
					curC = cost
				}
				if cost < bestC {
					bestK, bestC = k, cost
				}
				if k < len(rest) {
					u := rest[k]
					cost += w[[2]string{u, v}] - w[[2]string{v, u}]
				}
			}
			if bestC < curC {
				copy(order, rest[:bestK])
				order[bestK] = v
				copy(order[bestK+1:], rest[bestK:])
				changed = true
			}
		}
		if !changed {
			return
		}
	}
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

// groupWeights sums the references between groups.
func (a *Analysis) groupWeights() map[[2]string]int {
	w := map[[2]string]int{}
	for _, r := range a.ld.Refs {
		w[[2]string{r.FromGroup, r.ToGroup}]++
	}
	return w
}

// withOrder returns a copy of a that judges up and down by order.
func (a *Analysis) withOrder(order []string) *Analysis {
	b := *a
	b.order = order
	b.pos = map[string]int{}
	for i, g := range order {
		b.pos[g] = i
	}
	return &b
}

// SuggestOrder returns a copy of a ordered to minimise upward references,
// with the kernel group kept first.
func (a *Analysis) SuggestOrder() *Analysis {
	nodes := a.groupNames()
	pinned := ""
	if a.hasGroup("kernel") {
		pinned = "kernel"
	}
	return a.withOrder(suggestOrder(nodes, a.groupWeights(), pinned))
}

// Affinity is one group's size and the groups it exchanges the most references with.
type Affinity struct {
	Name  string         `json:"name"`
	Files int            `json:"files"`
	Lines int            `json:"lines"`
	Refs  int            `json:"refs"` // in plus out
	Top   []AffinityPeer `json:"top"`
}

// AffinityPeer is a group and the references exchanged with it in both directions.
type AffinityPeer struct {
	Group string  `json:"group"`
	Refs  int     `json:"refs"`
	Share float64 `json:"share"`
}

// Affinities lists every group, in the analysis's order, with its top 3 peers.
func (a *Analysis) Affinities() []Affinity {
	ex := map[string]map[string]int{}
	tot := map[string]int{}
	for _, r := range a.ld.Refs {
		for _, p := range [][2]string{{r.FromGroup, r.ToGroup}, {r.ToGroup, r.FromGroup}} {
			if ex[p[0]] == nil {
				ex[p[0]] = map[string]int{}
			}
			ex[p[0]][p[1]]++
			tot[p[0]]++
		}
	}
	var out []Affinity
	for _, g := range a.groupNames() {
		x := Affinity{Name: g, Files: a.files[g], Lines: a.lines[g], Refs: tot[g]}
		for o, n := range ex[g] {
			x.Top = append(x.Top, AffinityPeer{Group: o, Refs: n, Share: pct(n, tot[g])})
		}
		sort.Slice(x.Top, func(i, j int) bool {
			if x.Top[i].Refs != x.Top[j].Refs {
				return x.Top[i].Refs > x.Top[j].Refs
			}
			return x.Top[i].Group < x.Top[j].Group
		})
		if len(x.Top) > 3 {
			x.Top = x.Top[:3]
		}
		out = append(out, x)
	}
	return out
}
