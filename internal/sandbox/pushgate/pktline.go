package pushgate

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// Git's pkt-line framing: four hex digits give the length of the line,
// those four included; "0000" is a flush.

const maxPkt = 65520

var errFlush = errors.New("flush")

// readPkt reads one pkt-line; errFlush means a flush packet.
func readPkt(r *bufio.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n, err := strconv.ParseUint(string(hdr[:]), 16, 16)
	if err != nil {
		return nil, fmt.Errorf("bad pkt-line length %q", hdr[:])
	}
	switch {
	case n == 0:
		return nil, errFlush
	case n < 4 || n > maxPkt:
		return nil, fmt.Errorf("bad pkt-line length %d", n)
	}
	buf := make([]byte, n-4)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// pkt frames one line.
func pkt(s string) []byte {
	return append([]byte(fmt.Sprintf("%04x", len(s)+4)), s...)
}

var flushPkt = []byte("0000")

// reportWriter writes a receive-pack result: plain pkt-lines, or, when the
// client asked for side-band-64k, the report in band 1 and notes to the
// person in band 2 ("remote: …").
type reportWriter struct {
	w        io.Writer
	sideband bool
	quiet    bool
}

// note writes a line the client shows as "remote: …". Without side-band
// there is nowhere to put it, so it is dropped.
func (rw *reportWriter) note(s string) {
	if !rw.sideband || rw.quiet {
		return
	}
	for len(s) > 0 {
		chunk := s
		if len(chunk) > maxPkt-5-1 {
			chunk = chunk[:maxPkt-5-1]
		}
		s = s[len(chunk):]
		_, _ = rw.w.Write(pkt("\x02" + chunk + "\n"))
	}
}

// report writes the report-status lines and the flush that ends them.
func (rw *reportWriter) report(lines []string) {
	var body []byte
	for _, l := range lines {
		body = append(body, pkt(l+"\n")...)
	}
	body = append(body, flushPkt...)
	if !rw.sideband {
		_, _ = rw.w.Write(body)
		return
	}
	for len(body) > 0 {
		n := min(len(body), maxPkt-5)
		_, _ = rw.w.Write(pkt("\x01" + string(body[:n])))
		body = body[n:]
	}
	_, _ = rw.w.Write(flushPkt)
}
