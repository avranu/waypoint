package main

import (
	"encoding/json"
	"os"
	"testing"
)

// TestTraceDumpLocal parses the transcripts of this machine without
// uploading or remembering anything, and writes the trace rows as JSON lines
// to $WP_TRACE_DUMP, to compare with an offline analysis. Off by default.
func TestTraceDumpLocal(t *testing.T) {
	out := os.Getenv("WP_TRACE_DUMP")
	if out == "" {
		t.Skip("set WP_TRACE_DUMP=file to dump the local transcripts' traces")
	}
	files, err := discoverTranscripts(claudeDir(""), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	fh, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fh.Close() }()
	enc := json.NewEncoder(fh)
	repos := newRepoResolver()
	for _, f := range files {
		if !f.Main {
			continue
		}
		p, err := readUsageFile(f, usageFileState{}, repos.of, false)
		if err != nil {
			continue
		}
		for _, r := range p.Chunk.Traces {
			_ = enc.Encode(r)
		}
	}
}
