package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ledgerLine(id string, at time.Time, in, cw5, cw1, cr int64) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"id":%q,"model":"claude-opus-5-5","usage":{"input_tokens":%d,"output_tokens":5,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,"cache_creation":{"ephemeral_5m_input_tokens":%d,"ephemeral_1h_input_tokens":%d}},"content":[]}}`,
		at.Format(time.RFC3339Nano), id, in, cw5+cw1, cr, cw5, cw1)
}

func writeLedgerFile(t *testing.T, name string, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func sumLedger(rows []usageLedgerRow) usageLedgerRow {
	var s usageLedgerRow
	for _, r := range rows {
		s.Calls += r.Calls
		s.Ctx += r.Ctx
		s.Over200 += r.Over200
		s.Over300 += r.Over300
		s.CR300 += r.CR300
		s.Cold += r.Cold
		s.ColdW5 += r.ColdW5
		s.ColdW1 += r.ColdW1
		s.Forks += r.Forks
		s.ForkCtx += r.ForkCtx
		s.Fresh += r.Fresh
		s.FreshCtx += r.FreshCtx
	}
	return s
}

func TestUsageLedgerContextAndColdStarts(t *testing.T) {
	t0 := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	path := writeLedgerFile(t, "s.jsonl",
		ledgerLine("m1", t0, 10, 100, 0, 90),
		ledgerLine("m2", t0.Add(time.Minute), 10, 0, 0, 250_000),
		ledgerLine("m3", t0.Add(11*time.Minute), 5, 310_000, 0, 1000),
		ledgerLine("m4", t0.Add(50*time.Minute), 5, 0, 20, 0),
		ledgerLine("m5", t0.Add(90*time.Minute), 5, 0, 30, 0), // a one-hour cache write keeps the cache warm for an hour, not five minutes
	)
	f := usageFile{Path: path, Session: "s", Kind: "main", Main: true}
	p, err := readUsageFile(f, usageFileState{}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	l := sumLedger(p.Chunk.Ledger)
	if l.Calls != 5 || l.Over200 != 2 || l.Over300 != 1 || l.CR300 != 1000 {
		t.Errorf("context: %+v", l)
	}
	if l.Cold != 2 || l.ColdW5 != 310_000 || l.ColdW1 != 20 {
		t.Errorf("cold: %+v", l)
	}
	if want := int64(200 + 250_010 + 311_005 + 25 + 35); l.Ctx != want {
		t.Errorf("ctx %d, want %d", l.Ctx, want)
	}
	if l.Forks+l.Fresh != 0 {
		t.Errorf("a main session spawns nothing: %+v", l)
	}
}

func TestUsageLedgerSubagentsForkOrFresh(t *testing.T) {
	t0 := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	fork := writeLedgerFile(t, "agent-a.jsonl",
		`{"type":"fork-context-ref","agentId":"a","parentSessionId":"s","parentLastUuid":"u","contextLength":585}`,
		ledgerLine("f1", t0, 5, 0, 0, 280_000), ledgerLine("f2", t0.Add(time.Minute), 5, 10, 0, 280_000))
	fresh := writeLedgerFile(t, "agent-b.jsonl", ledgerLine("g1", t0, 5, 20_000, 0, 0))
	pf, err := readUsageFile(usageFile{Path: fork, Session: "s", Kind: "Explore"}, usageFileState{}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	pg, err := readUsageFile(usageFile{Path: fresh, Session: "s", Kind: "Explore"}, usageFileState{}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if l := sumLedger(pf.Chunk.Ledger); l.Forks != 1 || l.ForkCtx != 280_005 || l.Fresh != 0 || l.Calls != 2 {
		t.Errorf("fork: %+v", l)
	}
	if l := sumLedger(pg.Chunk.Ledger); l.Fresh != 1 || l.FreshCtx != 20_005 || l.Forks != 0 {
		t.Errorf("fresh: %+v", l)
	}
	more, err := os.OpenFile(fork, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = more.WriteString(ledgerLine("f3", t0.Add(2*time.Minute), 5, 0, 0, 281_000) + "\n")
	_ = more.Close()
	pf2, err := readUsageFile(usageFile{Path: fork, Session: "s", Kind: "Explore"}, pf.State, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if l := sumLedger(pf2.Chunk.Ledger); l.Forks != 0 || l.Calls != 1 {
		t.Errorf("resumed: %+v", l)
	}
}

func TestUsageLedgerRereadsOldHistoryForLedgerRowsOnly(t *testing.T) {
	t0 := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	path := writeLedgerFile(t, "s.jsonl", ledgerLine("m1", t0, 10, 100, 0, 90), ledgerLine("m2", t0.Add(time.Minute), 10, 0, 0, 300))
	f := usageFile{Path: path, Session: "s", Kind: "main", Main: true}
	fresh, err := readUsageFile(f, usageFileState{}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.LedgerOnly || len(fresh.Chunk.Messages) == 0 {
		t.Fatalf("a new file is read whole: %+v", fresh.Chunk)
	}
	old := fresh.State
	old.LV = 0
	again, err := readUsageFile(f, old, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if !again.LedgerOnly || len(again.Chunk.Messages) != 0 || len(again.Chunk.Tools) != 0 || len(again.Chunk.Sessions) != 0 || len(again.Chunk.Ledger) == 0 {
		t.Errorf("ledger only: %+v", again.Chunk)
	}
	if again.State.LV != usageLedgerVersion || again.From != 0 {
		t.Errorf("state: %+v from %d", again.State, again.From)
	}
	if chunkID("m", again) == chunkID("m", fresh) {
		t.Error("the re-read must not reuse the first read's chunk id")
	}
	third, err := readUsageFile(f, again.State, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if third.Chunk.rows() != 0 {
		t.Errorf("third: %+v", third.Chunk)
	}
}
