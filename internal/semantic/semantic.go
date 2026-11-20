// Package semantic learns which words go together from the text a server
// holds, so related things can be found when they share no words: "king"
// near "queen" because both turn up beside "crown" and "palace". It is
// latent semantic analysis (a truncated SVD of the tf-idf term matrix),
// in pure Go, deterministic for the same documents, and retrained as the
// text grows; no model is downloaded and nothing leaves the server
// (WP-779).
package semantic

import (
	"context"
	"math"
	"math/rand/v2"
	"sort"
)

// Options shape a model. Zero values take the defaults measured on
// Waypoint's own issues (WP-779): 100 dimensions, two power iterations,
// terms in at least two documents, each dimension weighted by the square
// root of its singular value.
type Options struct {
	K     int     // dimensions kept
	Iters int     // power iterations of the randomized SVD
	MinDF int     // documents a term must be in to have a place
	Scale float64 // documents are S^Scale in the latent space
	Seed  uint64
}

func (o Options) withDefaults() Options {
	if o.K <= 0 {
		o.K = 100
	}
	if o.Iters <= 0 {
		o.Iters = 2
	}
	if o.MinDF <= 0 {
		o.MinDF = 2
	}
	if o.Scale == 0 {
		o.Scale = 1.5
	}
	if o.Seed == 0 {
		o.Seed = 42
	}
	return o
}

// Model places terms, and through them any text, in a space where things
// written about together are near.
type Model struct {
	n    int                  // documents trained on
	df   map[string]int       // documents each known term is in
	rows map[string][]float64 // a term's fold-in row: V·S^(Scale-1)
	sing []float64            // singular values
	back []float64            // S^(2-Scale): a row times this is the term's place, V·S
}

// Docs is how many documents the model learned from.
func (m *Model) Docs() int { return m.n }

// Terms is how many terms the model knows.
func (m *Model) Terms() int { return len(m.rows) }

// Dims is the number of dimensions vectors have.
func (m *Model) Dims() int { return len(m.sing) }

// Train learns a model from documents given as term counts. Document
// order and the seed fix the result, so the same corpus gives the same
// model. It returns nil when nothing is learnable (no term in two
// documents).
func Train(ctx context.Context, docs []map[string]int, o Options) (*Model, error) {
	o = o.withDefaults()
	df := map[string]int{}
	for _, d := range docs {
		for t, c := range d {
			if c > 0 {
				df[t]++
			}
		}
	}
	var vocab []string
	for t, n := range df {
		if n >= o.MinDF {
			vocab = append(vocab, t)
		}
	}
	sort.Strings(vocab)
	nd, nt := len(docs), len(vocab)
	if nt < 2 || nd < 2 {
		return nil, nil
	}
	col := make(map[string]int, nt)
	for j, t := range vocab {
		col[t] = j
	}
	m := &Model{n: nd, df: make(map[string]int, nt), rows: make(map[string][]float64, nt)}
	for _, t := range vocab {
		m.df[t] = df[t]
	}
	// The doc-term matrix A, as sparse rows of unit tf-idf vectors. The
	// norm counts every term, the rows keep the known ones.
	type ent struct {
		j int
		w float64
	}
	rows := make([][]ent, nd)
	for i, d := range docs {
		v := weigh(d, df, nd)
		for t, w := range v {
			if j, ok := col[t]; ok {
				rows[i] = append(rows[i], ent{j, w})
			}
		}
		sort.Slice(rows[i], func(a, b int) bool { return rows[i][a].j < rows[i][b].j })
	}
	// At most a quarter of the matrix's smaller side: a model that keeps
	// every dimension of a small corpus learns no word's neighbours.
	k := min(o.K, max(1, min(nd, nt)/4))
	p := min(k+10, nd, nt)
	// mulAt: Aᵀ·X (nd×p → nt×p); mulA: A·Y (nt×p → nd×p).
	mulAt := func(X [][]float64) [][]float64 {
		Y := matrix(nt, p)
		for i, r := range rows {
			xi := X[i]
			for _, e := range r {
				y := Y[e.j]
				for c := range xi {
					y[c] += e.w * xi[c]
				}
			}
		}
		return Y
	}
	mulA := func(Y [][]float64) [][]float64 {
		X := matrix(nd, p)
		for i, r := range rows {
			x := X[i]
			for _, e := range r {
				y := Y[e.j]
				for c := range x {
					x[c] += e.w * y[c]
				}
			}
		}
		return X
	}
	rng := rand.New(rand.NewPCG(o.Seed, uint64(nd))) // #nosec G404 G115 -- a fixed sketch for repeatable models, not secrets
	omega := matrix(nd, p)
	for _, r := range omega {
		for c := range r {
			r[c] = rng.NormFloat64()
		}
	}
	// Randomized range finder (Halko et al.): an orthonormal basis Y of
	// A's row space, sharpened by power iterations.
	Y := mulAt(omega)
	orthonormalize(Y)
	for it := 0; it < o.Iters; it++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		X := mulA(Y)
		orthonormalize(X)
		Y = mulAt(X)
		orthonormalize(Y)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The right singular vectors in Y's basis: eigenvectors of (AY)ᵀ(AY).
	C := mulA(Y)
	G := matrix(p, p)
	for _, r := range C {
		for a := 0; a < p; a++ {
			if r[a] == 0 {
				continue
			}
			for b := 0; b < p; b++ {
				G[a][b] += r[a] * r[b]
			}
		}
	}
	vals, vecs := jacobi(G)
	order := make([]int, p)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return vals[order[a]] > vals[order[b]] })
	m.sing = make([]float64, k)
	scale := make([]float64, k)
	m.back = make([]float64, k)
	for c := 0; c < k; c++ {
		m.sing[c] = math.Sqrt(math.Max(vals[order[c]], 0))
		if m.sing[c] > 1e-12 {
			scale[c] = math.Pow(m.sing[c], o.Scale-1)
			m.back[c] = math.Pow(m.sing[c], 2-o.Scale)
		}
	}
	for j, t := range vocab {
		row := make([]float64, k)
		for c := 0; c < k; c++ {
			e := order[c]
			var s float64
			for a := 0; a < p; a++ {
				s += Y[j][a] * vecs[a][e]
			}
			row[c] = s * scale[c]
		}
		m.rows[t] = row
	}
	return m, nil
}

// weigh is a unit tf-idf vector: (1+log tf)·log(N/df).
func weigh(c map[string]int, df map[string]int, n int) map[string]float64 {
	v := make(map[string]float64, len(c))
	var norm float64
	terms := make([]string, 0, len(c))
	for t := range c {
		terms = append(terms, t)
	}
	sort.Strings(terms) // a fixed order keeps the norm bit-identical
	for _, t := range terms {
		k := c[t]
		if k <= 0 {
			continue
		}
		d := max(df[t], 1)
		w := (1 + math.Log(float64(k))) * math.Log(float64(max(n, d))/float64(d))
		if w > 0 {
			v[t] = w
			norm += w * w
		}
	}
	norm = math.Sqrt(norm)
	for t := range v {
		v[t] /= norm
	}
	return v
}

// Project places text, given as term counts, in the model's space as a
// unit vector. It is nil when the model knows none of the terms, and
// for a nil model.
func (m *Model) Project(counts map[string]int) []float64 {
	if m == nil {
		return nil
	}
	out := make([]float64, len(m.sing))
	known := false
	terms := make([]string, 0, len(counts))
	for t := range counts {
		terms = append(terms, t)
	}
	sort.Strings(terms) // a fixed order of sums: the same text, the same vector
	for _, t := range terms {
		row, ok := m.rows[t]
		if !ok || counts[t] <= 0 {
			continue
		}
		w := (1 + math.Log(float64(counts[t]))) * math.Log(float64(max(m.n, m.df[t]))/float64(m.df[t]))
		if w <= 0 {
			continue
		}
		known = true
		for x := range out {
			out[x] += w * row[x]
		}
	}
	if !known || !unit(out) {
		return nil
	}
	return out
}

// TermCos is how alike two terms are in the model, -1..1; 0 when either
// is unknown.
func (m *Model) TermCos(a, b string) float64 {
	if m == nil {
		return 0
	}
	ra, ok := m.rows[a]
	rb, ok2 := m.rows[b]
	if !ok || !ok2 {
		return 0
	}
	// Rows hold V·S^(Scale-1), weighted for folding text in; a term's own
	// place is V·S.
	x := make([]float64, len(ra))
	y := make([]float64, len(rb))
	for c := range ra {
		x[c], y[c] = ra[c]*m.back[c], rb[c]*m.back[c]
	}
	if !unit(x) || !unit(y) {
		return 0
	}
	return Cos(x, y)
}

// Cos is the cosine of two unit vectors; 0 when either is nil.
func Cos(a, b []float64) float64 {
	if a == nil || b == nil || len(a) != len(b) {
		return 0
	}
	var s float64
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

func unit(v []float64) bool {
	var n float64
	for _, x := range v {
		n += x * x
	}
	if n < 1e-24 {
		return false
	}
	n = math.Sqrt(n)
	for i := range v {
		v[i] /= n
	}
	return true
}

func matrix(r, c int) [][]float64 {
	backing := make([]float64, r*c)
	m := make([][]float64, r)
	for i := range m {
		m[i] = backing[i*c : (i+1)*c : (i+1)*c]
	}
	return m
}

// orthonormalize makes M's columns orthonormal by modified Gram-Schmidt
// (twice, for the precision a sketch with power iterations needs). A
// column with nothing left is zeroed.
func orthonormalize(M [][]float64) {
	if len(M) == 0 {
		return
	}
	p := len(M[0])
	for c := 0; c < p; c++ {
		for pass := 0; pass < 2; pass++ {
			for d := 0; d < c; d++ {
				var dot float64
				for i := range M {
					dot += M[i][c] * M[i][d]
				}
				for i := range M {
					M[i][c] -= dot * M[i][d]
				}
			}
		}
		var n float64
		for i := range M {
			n += M[i][c] * M[i][c]
		}
		n = math.Sqrt(n)
		for i := range M {
			if n < 1e-10 {
				M[i][c] = 0
			} else {
				M[i][c] /= n
			}
		}
	}
}

// jacobi is the eigendecomposition of a small symmetric matrix by cyclic
// Jacobi rotations: eigenvalues, and eigenvectors as columns.
func jacobi(A [][]float64) ([]float64, [][]float64) {
	n := len(A)
	a := matrix(n, n)
	v := matrix(n, n)
	for i := range a {
		copy(a[i], A[i])
		v[i][i] = 1
	}
	for sweep := 0; sweep < 100; sweep++ {
		var off, diag float64
		for i := 0; i < n; i++ {
			diag += a[i][i] * a[i][i]
			for j := i + 1; j < n; j++ {
				off += a[i][j] * a[i][j]
			}
		}
		if off <= 1e-22*math.Max(diag, 1e-300) {
			break
		}
		for p := 0; p < n; p++ {
			for q := p + 1; q < n; q++ {
				if math.Abs(a[p][q]) < 1e-300 {
					continue
				}
				theta := (a[q][q] - a[p][p]) / (2 * a[p][q])
				t := 1 / (math.Abs(theta) + math.Sqrt(theta*theta+1))
				if theta < 0 {
					t = -t
				}
				c := 1 / math.Sqrt(t*t+1)
				s := t * c
				for k := 0; k < n; k++ {
					akp, akq := a[k][p], a[k][q]
					a[k][p] = c*akp - s*akq
					a[k][q] = s*akp + c*akq
				}
				for k := 0; k < n; k++ {
					apk, aqk := a[p][k], a[q][k]
					a[p][k] = c*apk - s*aqk
					a[q][k] = s*apk + c*aqk
				}
				for k := 0; k < n; k++ {
					vkp, vkq := v[k][p], v[k][q]
					v[k][p] = c*vkp - s*vkq
					v[k][q] = s*vkp + c*vkq
				}
			}
		}
	}
	vals := make([]float64, n)
	for i := range vals {
		vals[i] = a[i][i]
	}
	return vals, v
}
