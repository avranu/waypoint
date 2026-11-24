package core

import (
	"context"
	"strings"
	"sync"
	"time"
	"unicode"

	"git.jmann.me/claude/waypoint/internal/semantic"
)

// boilerShingle words in a row, in boilerMinDocs issues or more, are
// boilerplate. Measured on 3,115 issues (WP-779): with these, pairs
// scoring as related were judged related 83% of the time, against 36%
// with the templates counted.
const (
	boilerShingle = 4
	boilerMinDocs = 20
)

// boilerplate is the set of shingles (hashed) that templates repeat.
type boilerplate map[uint64]bool

// simTokens are text's words before any are dropped: no issue keys or URLs.
func simTokens(text string) []string {
	return wordTokens(proseKeyRE.ReplaceAllString(linkURLRE.ReplaceAllString(text, " "), " "))
}

// shingles hashes each run of boilerShingle words; numbers count as one
// word, so "audit #12" and "audit #13" are the same template.
func shingles(toks []string) []uint64 {
	if len(toks) < boilerShingle {
		return nil
	}
	th := make([]uint64, len(toks))
	for i, t := range toks {
		th[i] = tokenHash(t)
	}
	out := make([]uint64, 0, len(toks)-boilerShingle+1)
	for i := 0; i+boilerShingle <= len(toks); i++ {
		h := uint64(14695981039346656037)
		for _, x := range th[i : i+boilerShingle] {
			h = (h ^ x) * 1099511628211
			h ^= h >> 29
		}
		out = append(out, h)
	}
	return out
}

// tokenHash is FNV-1a of a word, or of "0" for one with a digit in it.
func tokenHash(t string) uint64 {
	if strings.IndexFunc(t, unicode.IsDigit) >= 0 {
		t = "0"
	}
	h := uint64(14695981039346656037)
	for i := 0; i < len(t); i++ {
		h = (h ^ uint64(t[i])) * 1099511628211
	}
	return h
}

// boilerCounter counts the documents each shingle is in.
type boilerCounter map[uint64]int

// add counts one document, given as the shingles of its texts (an issue's
// title and description).
func (b boilerCounter) add(texts ...[]uint64) {
	b.count(1, texts)
}

// remove takes back a document add counted.
func (b boilerCounter) remove(texts ...[]uint64) {
	b.count(-1, texts)
}

func (b boilerCounter) count(by int, texts [][]uint64) {
	seen := map[uint64]bool{}
	for _, sh := range texts {
		for _, h := range sh {
			if seen[h] {
				continue
			}
			seen[h] = true
			if b[h] += by; b[h] <= 0 {
				delete(b, h)
			}
		}
	}
}

func (b boilerCounter) boilerplate() boilerplate {
	out := boilerplate{}
	for h, n := range b {
		if n >= boilerMinDocs {
			out[h] = true
		}
	}
	return out
}

// strip drops the words a boilerplate run covers; sh are toks' shingles.
func (b boilerplate) strip(toks []string, sh []uint64) []string {
	if len(b) == 0 {
		return toks
	}
	drop := make([]bool, len(toks))
	hit := false
	for i, h := range sh {
		if b[h] {
			hit = true
			for j := i; j < i+boilerShingle; j++ {
				drop[j] = true
			}
		}
	}
	if !hit {
		return toks
	}
	out := make([]string, 0, len(toks))
	for i, t := range toks {
		if !drop[i] {
			out = append(out, t)
		}
	}
	return out
}

const (
	// conceptMinDocs: fewer issues than this teach a model nothing
	// trustworthy; similarity stays on words alone.
	conceptMinDocs = 200
	// conceptRetrain: a model is retrained when it is this old and the
	// text changed, or sooner when the corpus grew by a quarter.
	conceptRetrain = 24 * time.Hour
	// conceptDims: dimensions of the model, as measured (WP-779).
	conceptDims = 100
)

type conceptState struct {
	mu       sync.Mutex
	model    *semantic.Model
	gen      int
	key      string
	docs     int64
	at       time.Time
	tried    time.Time
	training bool
}

// conceptRetry: the least time between two starts of training.
const conceptRetry = 10 * time.Minute

// concepts is the current model and its generation; nil before one is
// trained.
func (s *Service) concepts() (*semantic.Model, int) {
	s.meaning.mu.Lock()
	defer s.meaning.mu.Unlock()
	return s.meaning.model, s.meaning.gen
}

// wantConcepts starts training in the background when there is no model
// yet, or the one there is has gone stale, for a corpus of n issues with
// this key.
func (s *Service) wantConcepts(n int64, key string) {
	if n < conceptMinDocs {
		return
	}
	c := &s.meaning
	c.mu.Lock()
	stale := c.model == nil ||
		(key != c.key && s.now().Sub(c.at) >= conceptRetrain) ||
		n >= c.docs+c.docs/4
	if !stale || c.training || s.now().Sub(c.tried) < conceptRetry || s.bgCtx.Err() != nil {
		c.mu.Unlock()
		return
	}
	c.training, c.tried = true, s.now()
	c.mu.Unlock()
	s.goBG(func() {
		err := s.trainConcepts(s.bgCtx, key, conceptMinDocs)
		if err != nil {
			s.log.Info("concept model not trained", "err", err)
		}
		c.mu.Lock()
		c.training = false
		c.mu.Unlock()
	})
}

// trainConcepts trains a model on published issues (boilerplate left out)
// and the glossary, and makes it current. Fewer than minDocs issues train
// nothing.
func (s *Service) trainConcepts(ctx context.Context, key string, minDocs int) error {
	start := time.Now()
	rows, err := s.db.R.QueryContext(ctx, `SELECT title, description FROM issues WHERE draft = 0 AND outside = 0 ORDER BY id`)
	if err != nil {
		return err
	}
	var texts []*simText
	bc := boilerCounter{}
	for rows.Next() {
		var title, desc string
		if err := rows.Scan(&title, &desc); err != nil {
			rows.Close()
			return err
		}
		t := newSimText(title, desc)
		bc.add(t.sh[0], t.sh[1])
		texts = append(texts, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(texts) < minDocs {
		return nil
	}
	bp := bc.boilerplate()
	docs := make([]map[string]int, 0, len(texts))
	for _, t := range texts {
		docs = append(docs, simCountsOf(t.title, t.desc, t.sh, bp, nil))
	}
	gl, err := s.db.R.QueryContext(ctx, `SELECT term, aliases, definition FROM glossary ORDER BY id`)
	if err != nil {
		return err
	}
	for gl.Next() {
		var term, aliases, def string
		if err := gl.Scan(&term, &aliases, &def); err != nil {
			gl.Close()
			return err
		}
		docs = append(docs, senseCounts(term, aliases, def))
	}
	gl.Close()
	if err := gl.Err(); err != nil {
		return err
	}
	m, err := semantic.Train(ctx, docs, semantic.Options{K: conceptDims})
	if err != nil || m == nil {
		return err
	}
	c := &s.meaning
	c.mu.Lock()
	c.model, c.key, c.docs, c.at = m, key, int64(len(texts)), s.now()
	c.gen++
	c.mu.Unlock()
	s.log.Info("concept model trained", "issues", len(texts), "glossary", len(docs)-len(texts), "terms", m.Terms(), "took", time.Since(start).Round(time.Millisecond))
	return nil
}

// A glossary word is linked where it's used in the sense its entry
// defines (WP-779): the words around it (senseWindow either side) must
// be near the entry's term and definition in the concept model. Measured
// on 160 judged uses: exact matching alone was right 60% of the time,
// with the sense check 87%, keeping 79% of the right ones ("session" for
// an agent's run no longer links the sign-in entry).
const (
	senseWindow = 8
	senseMin    = 0.5
)

// senseCounts are a glossary entry's words: its term and aliases weighted
// like a title, and its definition.
func senseCounts(term, aliases, definition string) map[string]int {
	return simCounts(term+" "+strings.Join(jsonStrings(aliases), " "), definition, nil)
}

// senseGate says whether a glossary word at toks[at:at+n] is used in its
// entry's sense. With no model, or an entry or window the model knows
// nothing of, every use counts, as before.
type senseGate func(toks []string, at, n int, entry int64) bool

// senseGate returns the check for idx's entries under the current model,
// or nil when there is none.
func (s *Service) senseGate(idx *termIndex) senseGate {
	m, gen := s.concepts()
	if m == nil || idx == nil {
		return nil
	}
	senses := idx.sensesFor(m, gen)
	return func(toks []string, at, n int, entry int64) bool {
		sense := senses[entry]
		if sense == nil {
			return true
		}
		lo, hi := max(0, at-senseWindow), min(len(toks), at+n+senseWindow)
		win := m.Project(simCounts("", strings.Join(toks[lo:hi], " "), nil))
		return win == nil || semantic.Cos(win, sense) >= senseMin
	}
}

// sensesFor projects each entry's sense under model generation gen, once.
func (idx *termIndex) sensesFor(m *semantic.Model, gen int) map[int64][]float64 {
	idx.senseMu.Lock()
	defer idx.senseMu.Unlock()
	if idx.senses == nil || idx.senseGen != gen {
		idx.senses = make(map[int64][]float64, len(idx.sense))
		for id, c := range idx.sense {
			idx.senses[id] = m.Project(c)
		}
		idx.senseGen = gen
	}
	return idx.senses
}
