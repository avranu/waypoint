package transcript

import (
	"fmt"
	"os"
	"sort"
	"testing"
	"time"
)

// TestSampleFile parses the transcript named by WP_TRANSCRIPT_SAMPLE and prints
// what it found: rows by table, unknown records, time per MB. It is how the
// numbers in the pull request were measured. It asserts nothing: it prints
// figures; without the variable it skips.
func TestSampleFile(t *testing.T) {
	path := os.Getenv("WP_TRANSCRIPT_SAMPLE")
	if path == "" {
		t.Skip("WP_TRANSCRIPT_SAMPLE not set")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	start := time.Now()
	r, err := Parse(f, "main")
	if err != nil {
		t.Fatal(err)
	}
	el := time.Since(start)
	mb := float64(st.Size()) / (1 << 20)
	fmt.Printf("size %.1f MB lines %d parse %v (%.3f s/MB)\n", mb, r.Lines, el, el.Seconds()/mb)
	fmt.Printf("model_calls %d tool_calls %d turns %d compactions %d injected %d spawns %d settings %d\n", len(r.ModelCalls), len(r.ToolCalls),
		len(r.Turns), len(r.Compactions), len(r.Injected), len(r.Spawns), len(r.Settings))
	for _, m := range []map[string]int{r.Unknown, r.Ignored} {
		var ks []string
		for k := range m {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			fmt.Printf("  %s %d\n", k, m[k])
		}
		fmt.Println("--")
	}
}
