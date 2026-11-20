// Package lazyre holds package-level regexps that compile on first use, so a
// process start does not pay for patterns it never matches.
package lazyre

import (
	"fmt"
	"regexp"
	"sync"
)

// Regexp is a regexp that compiles on first use, safe for concurrent use.
// Compiling the few hundred patterns that core, wp and the others declared at
// init cost tens of milliseconds and ~17 MB on every process start, and every
// `wp hook` call pays process start under a 2 s deadline (WP-1398). Use New
// where a pattern would be regexp.MustCompile in a package-level var;
// patterns compiled inside a function are already lazy. A bad pattern panics
// at first use; CompileAll (run by each package's tests) compiles every one.
// Its methods are the *regexp.Regexp ones the code uses; add more as needed.
type Regexp struct {
	expr string
	once sync.Once
	re   *regexp.Regexp
}

var all struct {
	sync.Mutex
	list []*Regexp
}

func New(expr string) *Regexp {
	l := &Regexp{expr: expr}
	all.Lock()
	all.list = append(all.list, l)
	all.Unlock()
	return l
}

// CompileAll compiles every pattern declared so far and returns the first
// error, for a test that guards against a bad pattern surviving until first use.
func CompileAll() (err error) {
	all.Lock()
	list := append([]*Regexp(nil), all.list...)
	all.Unlock()
	for _, l := range list {
		func() {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("%v", r)
				}
			}()
			l.get()
		}()
		if err != nil {
			return err
		}
	}
	return nil
}

func (l *Regexp) get() *regexp.Regexp {
	l.once.Do(func() { l.re = regexp.MustCompile(l.expr) })
	return l.re
}

func (l *Regexp) String() string                 { return l.expr }
func (l *Regexp) MatchString(s string) bool      { return l.get().MatchString(s) }
func (l *Regexp) Match(b []byte) bool            { return l.get().Match(b) }
func (l *Regexp) FindString(s string) string     { return l.get().FindString(s) }
func (l *Regexp) FindStringIndex(s string) []int { return l.get().FindStringIndex(s) }
func (l *Regexp) FindStringSubmatch(s string) []string {
	return l.get().FindStringSubmatch(s)
}
func (l *Regexp) FindStringSubmatchIndex(s string) []int {
	return l.get().FindStringSubmatchIndex(s)
}
func (l *Regexp) FindAllString(s string, n int) []string { return l.get().FindAllString(s, n) }
func (l *Regexp) FindAllStringIndex(s string, n int) [][]int {
	return l.get().FindAllStringIndex(s, n)
}
func (l *Regexp) FindAllStringSubmatch(s string, n int) [][]string {
	return l.get().FindAllStringSubmatch(s, n)
}
func (l *Regexp) FindAllStringSubmatchIndex(s string, n int) [][]int {
	return l.get().FindAllStringSubmatchIndex(s, n)
}
func (l *Regexp) ReplaceAllString(s, repl string) string {
	return l.get().ReplaceAllString(s, repl)
}
func (l *Regexp) ReplaceAllLiteralString(s, repl string) string {
	return l.get().ReplaceAllLiteralString(s, repl)
}
func (l *Regexp) ReplaceAllStringFunc(s string, f func(string) string) string {
	return l.get().ReplaceAllStringFunc(s, f)
}
func (l *Regexp) Split(s string, n int) []string   { return l.get().Split(s, n) }
func (l *Regexp) SubexpNames() []string            { return l.get().SubexpNames() }
func (l *Regexp) SubexpIndex(name string) int      { return l.get().SubexpIndex(name) }
func (l *Regexp) NumSubexp() int                   { return l.get().NumSubexp() }
func (l *Regexp) Find(b []byte) []byte             { return l.get().Find(b) }
func (l *Regexp) FindSubmatch(b []byte) [][]byte   { return l.get().FindSubmatch(b) }
func (l *Regexp) ReplaceAll(b, repl []byte) []byte { return l.get().ReplaceAll(b, repl) }
func (l *Regexp) FindAll(b []byte, n int) [][]byte { return l.get().FindAll(b, n) }
