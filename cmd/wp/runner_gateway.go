package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/textproto"
	"os"
	"strings"
	"time"
)

const (
	gatewayCLI = "gateway"
	// gatewayArchiveMax is the most the gateway takes of a packed checkout,
	// and gatewayFileMax its limit for one file, which it would leave out
	// anyway: leaving it out here keeps it from counting against the first.
	gatewayArchiveMax = 20 << 20
	gatewayFileMax    = 5 << 20
	// gatewayAdmitWait is how long a job keeps offering its task to a
	// gateway that says busy (its queue is full, it is restarting) before
	// the audit goes to its next model.
	gatewayAdmitWait = 10 * time.Minute
	// gatewayUploadTimeout bounds one upload: to Waypoint, and from there
	// to the gateway, which answers once it has the task.
	gatewayUploadTimeout = 12 * time.Minute
	// gatewayResends is how often a task is sent again after the gateway
	// restarted under it (its kind "interrupted": safe to send again).
	gatewayResends = 2
	// A job's result is held to what the server takes (core.RunnerResultMax, 2 MB, and the helpers' runs share it): the answer's text (Waypoint keeps
	// 60 KB of it) and its findings as JSON.
	gatewayAnswerMax = 60 << 10
	gatewayJSONMax   = 180 << 10
)

// gatewayPollEvery is how often a job asks Waypoint how its task goes.
var gatewayPollEvery = 10 * time.Second

// gatewayTaskView is Waypoint's answer about the job's task.
type gatewayTaskView struct {
	ID           string          `json:"id"`
	Status       string          `json:"status"` // queued, running, succeeded, failed, cancelled; refused, unknown
	Model        string          `json:"model"`
	Text         string          `json:"text"`
	JSON         json.RawMessage `json:"json"`
	ErrorKind    string          `json:"error_kind"`
	Error        string          `json:"error"`
	RetryAfter   int             `json:"retry_after"`
	InputTokens  int64           `json:"input_tokens"`
	OutputTokens int64           `json:"output_tokens"`
	CostUSD      float64         `json:"cost_usd"`
}

// execGatewayAudit runs the audit's model at the gateway over the checkout
// in work. result holds what the job reports of the checkout already.
func (p *runnerProc) execGatewayAudit(ctx context.Context, run *jobRun, in agentJob, work string, result map[string]any) outcome {
	ended := func(view *gatewayTaskView) (outcome, bool) {
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			p.gatewayCancel(run)
			result["gateway_error"] = "timeout"
			if view != nil {
				result["gateway_task"] = view.ID
			}
			return outcome{State: "failed", Error: fmt.Sprintf("The audit through the gateway timed out after %s and was stopped.", run.timeout.Round(time.Second)), Result: result}, true
		case ctx.Err() != nil:
			p.gatewayCancel(run)
			return outcome{State: "cancelled", Error: "Stopped."}, true
		}
		return outcome{}, false
	}
	gave := func(view *gatewayTaskView, kind, format string, a ...any) outcome {
		result["gateway_error"] = kind
		if view != nil {
			result["gateway_task"], result["model"] = view.ID, view.Model
			result["answer"] = excerpt(view.Text, gatewayAnswerMax)
		}
		return outcome{State: "failed", Error: fmt.Sprintf(format, a...), Result: result}
	}

	archive, err := os.CreateTemp(p.cacheDir(), fmt.Sprintf("job%d-*.tar.gz", run.id))
	if err != nil {
		return failed("%v", err)
	}
	defer func() {
		_ = archive.Close()
		_ = os.Remove(archive.Name())
	}()
	files, left, err := packCheckout(work, archive, gatewayArchiveMax)
	if errors.Is(err, errArchiveTooLarge) {
		return gave(nil, "body_too_large", "The checkout packs to more than %d MiB, the most the gateway takes for an audit.", gatewayArchiveMax>>20)
	}
	if err != nil {
		return p.agentError(ctx, run, "can't pack the checkout: %v", err)
	}
	size, err := archive.Seek(0, io.SeekCurrent)
	if err != nil {
		return failed("%v", err)
	}
	fmt.Fprintf(run.out, "Packed the checkout for the gateway: %d file%s, %.1f MiB\n", files, pluralS(files), float64(size)/(1<<20))
	if len(left) > 0 {
		fmt.Fprintf(run.out, "Left out %d file%s over %d MiB (the gateway takes none that large): %s\n", len(left), pluralS(len(left)), gatewayFileMax>>20, excerpt(strings.Join(left, ", "), 300))
	}

	path := fmt.Sprintf("/api/v1/runner/jobs/%d/gateway", run.id)
	for sent := 0; ; sent++ {
		var view *gatewayTaskView
		for began := time.Now(); ; {
			view, err = p.gatewayUpload(ctx, path, in.Prompt, archive, size)
			if out, ok := ended(view); ok {
				return out
			}
			if err != nil {
				kind := "unreachable"
				var ae *apiError
				if errors.As(err, &ae) && ae.status >= 400 && ae.status < 500 {
					kind = "refused"
				}
				return gave(nil, kind, "Waypoint didn't take the audit for the gateway: %v", err)
			}
			if view.Status != "refused" {
				break
			}
			wait := time.Duration(view.RetryAfter) * time.Second
			if wait <= 0 || time.Since(began)+wait > gatewayAdmitWait {
				return gave(nil, orStr(view.ErrorKind, "server"), "The gateway didn't take the audit (%s): %s", orStr(view.ErrorKind, "no reason given"), view.Error)
			}
			fmt.Fprintf(run.out, "The gateway is busy (%s); offering the audit again in %s\n", orStr(view.ErrorKind, "no reason given"), wait)
			select {
			case <-ctx.Done():
			case <-time.After(wait):
			}
			if out, ok := ended(nil); ok {
				return out
			}
		}
		fmt.Fprintf(run.out, "The gateway took the audit as task %s (%s)\n", view.ID, view.Status)

		said, misses := view.Status, 0
		for view.Status == "queued" || view.Status == "running" || view.Status == "unknown" {
			select {
			case <-ctx.Done():
			case <-time.After(gatewayPollEvery):
			}
			if out, ok := ended(view); ok {
				return out
			}
			var next gatewayTaskView
			if err := p.c.callInto(ctx, "GET", path, nil, &next); err != nil {
				if out, ok := ended(view); ok {
					return out
				}
				var ae *apiError
				if errors.As(err, &ae) && ae.status >= 400 && ae.status < 500 {
					return failed("Waypoint no longer knows the audit's task: %v", err)
				}
				if misses++; misses == 1 {
					fmt.Fprintf(run.out, "Can't ask Waypoint about the task (%v); asking again\n", err)
				}
				continue
			}
			misses = 0
			if next.ID == "" {
				next.ID = view.ID
			}
			view = &next
			if view.Status != said && (view.Status == "queued" || view.Status == "running") {
				fmt.Fprintf(run.out, "Task %s is %s\n", view.ID, view.Status)
				said = view.Status
			}
		}
		switch {
		case view.Status == "succeeded" && len(view.JSON) > gatewayJSONMax:
			return gave(view, "output_limit", "The audit through the gateway answered %d KB of findings, more than a job's result holds.", len(view.JSON)>>10)
		case view.Status == "succeeded":
			fmt.Fprintf(run.out, "%s answered (%d tokens in, %d out)\n\n%s\n", orStr(view.Model, "The gateway"), view.InputTokens, view.OutputTokens, excerpt(view.Text, 4000))
			result["answer"], result["model"], result["gateway_task"] = excerpt(view.Text, gatewayAnswerMax), view.Model, view.ID
			result["tokens"] = tokenUse{Input: view.InputTokens, Output: view.OutputTokens, CostUSD: view.CostUSD}
			if js := bytes.TrimSpace(view.JSON); len(js) > 0 && string(js) != "null" {
				result["structured"] = view.JSON
			}
			return outcome{State: "done", Result: result}
		case view.ErrorKind == "interrupted" && sent < gatewayResends:
			fmt.Fprintf(run.out, "The gateway restarted under task %s; sending the audit again\n", view.ID)
		default:
			kind := orStr(view.ErrorKind, "server")
			return gave(view, kind, "The audit through the gateway %s (%s): %s", map[bool]string{true: "was cancelled there", false: "failed"}[view.Status == "cancelled"], kind, orStr(view.Error, "no reason given"))
		}
	}
}

// gatewayUpload hands Waypoint the prompt and the packed checkout for the
// job's task: multipart, the archive streamed from its file.
func (p *runnerProc) gatewayUpload(ctx context.Context, path, prompt string, archive *os.File, size int64) (*gatewayTaskView, error) {
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	var head bytes.Buffer
	mw := multipart.NewWriter(&head)
	if err := mw.WriteField("prompt", prompt); err != nil {
		return nil, err
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="repo"; filename="repo.tar.gz"`)
	h.Set("Content-Type", "application/gzip")
	if _, err := mw.CreatePart(h); err != nil {
		return nil, err
	}
	body := io.MultiReader(&head, io.LimitReader(archive, size), strings.NewReader("\r\n--"+mw.Boundary()+"--\r\n"))
	c := *p.c
	c.longPoll = gatewayUploadTimeout
	raw, err := c.send(ctx, "POST", path, mw.FormDataContentType(), body)
	if err != nil {
		return nil, err
	}
	var view gatewayTaskView
	if err := json.Unmarshal(raw, &view); err != nil {
		return nil, fmt.Errorf("can't read what Waypoint said of the task: %w", err)
	}
	return &view, nil
}

// gatewayCancel stops the job's task at the gateway after the job itself
// was stopped or ran out of time; the job's own context is over by then.
func (p *runnerProc) gatewayCancel(run *jobRun) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := p.c.callCtx(ctx, "POST", fmt.Sprintf("/api/v1/runner/jobs/%d/gateway/cancel", run.id), nil); err != nil && !isNotFound(err) {
		fmt.Fprintf(run.out, "Couldn't stop the task at the gateway (%v); it ends at its own time limit\n", err)
	}
}

var errArchiveTooLarge = errors.New("the archive is too large")

// limitWriter fails once more than max bytes were written through it.
type limitWriter struct {
	w    io.Writer
	left int64
}

func (l *limitWriter) Write(b []byte) (int, error) {
	if int64(len(b)) > l.left {
		return 0, errArchiveTooLarge
	}
	l.left -= int64(len(b))
	return l.w.Write(b)
}

// packCheckout writes the files of the checkout in work to w as a gzipped
// tar: regular files only (git's own directory, links and devices stay
// out, and nothing is followed out of work), none over gatewayFileMax, and
// no more than max bytes in all (errArchiveTooLarge). It answers how many
// files went in and which were left out for their size.
func packCheckout(work string, w io.Writer, max int64) (files int, left []string, err error) {
	root, err := os.OpenRoot(work)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = root.Close() }()
	zw := gzip.NewWriter(&limitWriter{w: w, left: max})
	tw := tar.NewWriter(zw)
	err = fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == ".git" {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > gatewayFileMax {
			left = append(left, path)
			return nil
		}
		f, err := root.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		mode := int64(0o644)
		if info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: path, Mode: mode, Size: info.Size()}); err != nil {
			return err
		}
		if _, err := io.CopyN(tw, f, info.Size()); err != nil {
			return err
		}
		files++
		return nil
	})
	if err == nil {
		err = tw.Close()
	}
	if err == nil {
		err = zw.Close()
	}
	return files, left, err
}
