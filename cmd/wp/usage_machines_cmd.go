package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

var usageMachinesCmd = &command{
	name: "machines", args: "[--user NAME]",
	summary: "each machine's transcript archive state: waiting, drops, passes, opt-out",
	about: `Lists the machines that report for you (an admin can name anyone with
--user) and what each says of its transcript archive: whether it is off
(WP_USAGE_ARCHIVE=off on that machine), the bytes still waiting to upload,
what was dropped and why, how long the last and the longest hook pass took (a
manual pass is not timed; the last one can be 12 hours old), why
the last pass stopped early (blank: it ran to the end), how many passes
were cut short since the machine began, and when its last piece was stored.
"unknown" means that machine has not said (an older wp, or never reported).`,
	examples: []string{"wp usage machines", "wp usage machines --user robin"},
	run:      cmdUsageMachines,
}

func init() {
	usageCmd.subs = append(usageCmd.subs, usageMachinesCmd)
}

func cmdUsageMachines(c *cli, args []string) error {
	fs := c.flags()
	user := fs.String("user", "", "admins: whose machines (a `USERNAME`)")
	if _, err := c.parse(fs, args); err != nil {
		return err
	}
	q := url.Values{}
	if *user != "" {
		q.Set("user", *user)
	}
	var ms []core.UsageMachine
	if err := c.do("GET", "/api/v1/usage/machines?"+q.Encode(), nil, &ms); err != nil {
		return err
	}
	raw, _ := json.Marshal(ms)
	return c.emit(raw, func() string { return usageMachinesText(ms) })
}

func msText(p *int64) string {
	if p == nil {
		return "unknown"
	}
	d := time.Duration(*p) * time.Millisecond
	if d >= time.Second {
		d = d.Round(time.Second)
	}
	return d.String()
}

func usageMachinesText(ms []core.UsageMachine) string {
	if len(ms) == 0 {
		return "No machines have reported."
	}
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "MACHINE\tARCHIVE\tWAITING\tDROPPED\tLAST HOOK PASS\tLONGEST HOOK PASS\tSTOPPED\tCUT SHORT\tLAST PIECE")
	for _, m := range ms {
		t := m.Transcripts
		if t == nil {
			_, _ = fmt.Fprintf(tw, "%s\tunknown\t-\t-\tunknown\tunknown\tunknown\tunknown\t-\n", m.Name)
			continue
		}
		state, stop, cut, last := "unknown", "unknown", "unknown", "-"
		switch {
		case t.Off != nil && *t.Off:
			state = "off"
		case t.Off != nil:
			state = "on"
		}
		if t.Stop != nil {
			if stop = *t.Stop; stop == "" {
				stop = "ran to the end"
			}
		}
		if t.Cut != nil {
			cut = fmt.Sprint(*t.Cut)
		}
		if t.LastPieceAt != nil {
			last = t.LastPieceAt.Local().Format("2006-01-02 15:04")
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", m.Name, state,
			fmt.Sprintf("%s in %d files", bytesText(t.PendingBytes), t.PendingFiles), droppedText(t.Dropped),
			msText(t.PassMS), msText(t.PassMaxMS), stop, cut, last)
	}
	_ = tw.Flush()
	return b.String()
}

// droppedText lists the drop reasons in words, "none" when there are none.
func droppedText(d map[string]int64) string {
	var parts []string
	for k, n := range d {
		if n > 0 {
			parts = append(parts, archiveDropText(k, n))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

// transcriptFilesText is the table `wp usage transcript` prints.
func transcriptFilesText(files []core.TranscriptFile) string {
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "MACHINE\tFILE\tKIND\tGEN\tPIECES\tRAW\tSTORED\tREDACTED\tREDACTIONS\tCOMPLETE\tLAST")
	for _, f := range files {
		red := "unknown"
		if f.Redactions != nil {
			red = fmt.Sprint(*f.Redactions)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%s\t%s\t%v\t%s\t%v\t%s\n", f.Machine, f.File, f.Kind, f.Gen, f.Pieces,
			bytesText(f.RawBytes), bytesText(f.StoredBytes), f.Redacted, red, f.Complete, time.UnixMilli(f.LastAt).Format("2006-01-02 15:04"))
	}
	_ = tw.Flush()
	return b.String()
}
