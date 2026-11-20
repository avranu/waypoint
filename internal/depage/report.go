package depage

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

// Report prints the results, problems first, and says whether every new
// version may land. verbose lists the ones that passed too. annotate adds a
// CI ::error line per problem, so the run's summary names it.
//
// Names, versions, paths and details come from the pushed tree (a
// lockfile's resolved URL, say), so each is printed through oneLine: none
// can end a line and start one CI's runner reads as a workflow command.
func Report(w io.Writer, results []Result, minAge time.Duration, verbose, annotate bool) bool {
	slices.SortStableFunc(results, func(a, b Result) int { return strings.Compare(a.String(), b.String()) })
	days := int(minAge.Hours() / 24)
	var bad, waived, stale []Result
	ok, retry := 0, 0
	for _, r := range results {
		switch {
		case !r.Passes():
			bad = append(bad, r)
			if r.Lookup {
				retry++
			}
		case r.Waiver != nil && r.Verdict == OK:
			stale = append(stale, r)
		case r.Waiver != nil:
			waived = append(waived, r)
		default:
			ok++
		}
	}
	_, _ = fmt.Fprintf(w, "dep-age: %d new dependency version(s), each must be %d days old: %d old enough, %d waived, %d not\n", len(results), days, ok+len(stale), len(waived), len(bad))
	for _, r := range bad {
		label := "TOO NEW"
		if r.Verdict == Unknown {
			label = "UNKNOWN"
		}
		_, _ = fmt.Fprintf(w, "  %-8s %s (%s): %s\n", label, oneLine(r.String()), oneLine(r.Where), oneLine(r.Detail))
		if annotate {
			_, _ = fmt.Fprintf(w, "::error file=%s,title=Dependency age::%s\n", cmdProperty(r.Where), cmdData(r.String()+" "+r.Detail))
		}
	}
	for _, r := range waived {
		_, _ = fmt.Fprintf(w, "  waived   %s (%s): %s; %s:%d says: %s\n", oneLine(r.String()), oneLine(r.Where), oneLine(r.Detail), WaiverFile, r.Waiver.Line, r.Waiver.Reason)
	}
	for _, r := range stale {
		_, _ = fmt.Fprintf(w, "  ok       %s: old enough now, so %s:%d can go\n", oneLine(r.String()), WaiverFile, r.Waiver.Line)
	}
	if verbose {
		for _, r := range results {
			if r.Verdict == OK && r.Waiver == nil {
				_, _ = fmt.Fprintf(w, "  ok       %s (%s): published %s\n", oneLine(r.String()), oneLine(r.Where), stamp(r.Published))
			}
		}
	}
	if len(bad) == 0 {
		return true
	}
	if retry > 0 {
		// A waiver would land a version whose age was never read, for good.
		_, _ = fmt.Fprintf(w, "\n%d version(s) UNKNOWN because a registry or Go service didn't answer: run it again (in CI, re-run the job) once it does.\n", retry)
		if retry == len(bad) {
			return false
		}
	}
	_, _ = fmt.Fprintf(w, "\nA version published in the last %d days may be a hijacked release no one has caught yet (WP-844). Pin one at least %d days old,\n", days, days)
	_, _ = fmt.Fprintf(w, "or wait. A security fix that can't wait can land with a reason, a line in %s per version:\n", WaiverFile)
	for _, r := range bad {
		if r.Lookup {
			continue
		}
		_, _ = fmt.Fprintf(w, "  %s %s %s REASON (the advisory it fixes, and why it can't wait)\n", r.Eco, oneLine(r.Name), oneLine(r.Version))
	}
	return false
}

// oneLine shows each control character in s as an escape (\x0a), so s
// stays on one line.
func oneLine(s string) string {
	if !strings.ContainsFunc(s, isControl) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if isControl(r) {
			_, _ = fmt.Fprintf(&b, `\x%02x`, r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isControl(r rune) bool {
	return r < 0x20 || r == 0x7f || r == 0x85 || r == 0x2028 || r == 0x2029
}

// cmdData escapes a workflow command's message as the runner unescapes it
// (%, CR and LF; oneLine has already removed the last two).
func cmdData(s string) string { return strings.ReplaceAll(oneLine(s), "%", "%25") }

// cmdProperty escapes a workflow command's property value: : and , too.
func cmdProperty(s string) string {
	return strings.NewReplacer(":", "%3A", ",", "%2C").Replace(cmdData(s))
}
