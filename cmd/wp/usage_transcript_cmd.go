package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// wp usage transcript (WP-1466): read back what the transcript archive holds.

var usageTranscriptCmd = &command{
	name: "transcript", args: "SESSION [--file FILE] [--machine M] [--gen N] [--user NAME]",
	summary: "list or read a session's archived transcript",
	about: `Without --file, lists the files the archive holds of the session (the main
thread, and each sub-agent's transcript under subagents/), per machine, with
their sizes, generation (a file Claude Code rewrote starts the next) and
whether redaction ran. With --file (main, or a path from the list) writes
that file's JSONL to stdout, every line whole, in the order it was written.
Your own sessions; an admin can read anyone's with --user. Redaction removes
credentials of known shapes, not everything private: treat the text as private.`,
	examples: []string{"wp usage transcript 0b1c…", "wp usage transcript 0b1c… --file main > session.jsonl"},
	run:      cmdUsageTranscript,
}

func init() {
	usageCmd.subs = append(usageCmd.subs, usageTranscriptCmd)
}

func cmdUsageTranscript(c *cli, args []string) error {
	fs := c.flags()
	file := fs.String("file", "", "write this `FILE`'s JSONL (main, or a path from the list)")
	machine := fs.String("machine", "", "only this `MACHINE`")
	gen := fs.Int("gen", -1, "a generation `N` of the file (default the newest)")
	user := fs.String("user", "", "admins: whose session (a `USERNAME`)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return c.usage("give the session id")
	}
	q := url.Values{"session": {pos[0]}}
	if *machine != "" {
		q.Set("machine", *machine)
	}
	if *user != "" {
		q.Set("user", *user)
	}
	if *gen >= 0 {
		q.Set("gen", fmt.Sprint(*gen))
	}
	if *file != "" {
		q.Set("file", *file)
		return c.streamGet("/api/v1/usage/transcript?"+q.Encode(), c.out)
	}
	var files []core.TranscriptFile
	if err := c.do("GET", "/api/v1/usage/transcripts?"+q.Encode(), nil, &files); err != nil {
		return err
	}
	raw, _ := json.Marshal(files)
	return c.emit(raw, func() string {
		if len(files) == 0 {
			return "Nothing archived for that session."
		}
		var b strings.Builder
		tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "MACHINE\tFILE\tKIND\tGEN\tPIECES\tRAW\tSTORED\tREDACTED\tCOMPLETE\tLAST")
		for _, f := range files {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%s\t%s\t%v\t%v\t%s\n", f.Machine, f.File, f.Kind, f.Gen, f.Pieces,
				bytesText(f.RawBytes), bytesText(f.StoredBytes), f.Redacted, f.Complete, time.UnixMilli(f.LastAt).Format("2006-01-02 15:04"))
		}
		_ = tw.Flush()
		return b.String()
	})
}

// streamGet copies a GET answer to w without the size cap or time limit of
// a JSON call: a transcript can be large.
func (c *cli) streamGet(path string, w io.Writer) error {
	req, err := http.NewRequest("GET", c.server()+path, nil) // #nosec G107 G704 -- the configured server
	if err != nil {
		return err
	}
	req.Header.Set("X-Waypoint-Client", "wp")
	req.Header.Set("User-Agent", "wp/"+version)
	if t := c.token(); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	resp, err := (&http.Client{}).Do(req) // #nosec G704 -- the configured server
	if err != nil {
		return fmt.Errorf("can't reach %s: %w", c.server(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(b, &e)
		return &apiError{status: resp.StatusCode, msg: e.Error, method: "GET", path: strings.SplitN(path, "?", 2)[0]}
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		return fmt.Errorf("the transcript stream broke (what was written is incomplete): %w", err)
	}
	return nil
}
