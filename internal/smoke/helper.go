package smoke

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// HelperRun runs `curl <args...>` in a throwaway container on the network the
// checks must be made from, and returns its stdout.
type HelperRun func(ctx context.Context, args ...string) ([]byte, error)

// HelperTransport is an http.RoundTripper that performs each request with
// curl inside the helper container. A rehearsal runs on an internal network
// whose published ports the host cannot reach, so the request has to start
// from within it.
func HelperTransport(run HelperRun) http.RoundTripper { return helperTransport{run} }

type helperTransport struct{ run HelperRun }

func maxTime(ctx context.Context) string {
	secs := 10
	if dl, ok := ctx.Deadline(); ok {
		if d := int(time.Until(dl).Seconds()) + 1; d > 0 && d < 120 {
			secs = d
		}
	}
	return strconv.Itoa(secs)
}

func (h helperTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet {
		return nil, fmt.Errorf("the rehearsal helper only does GET, not %s", req.Method)
	}
	out, err := h.run(req.Context(), "-sS", "-i", "--max-time", maxTime(req.Context()), req.URL.String())
	if err != nil {
		return nil, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(out)), req)
	if err != nil {
		return nil, fmt.Errorf("unreadable answer from the helper: %w", err)
	}
	return resp, nil
}

// HelperDial checks that a TCP connection to addr can be made, from inside
// the helper container.
func HelperDial(run HelperRun) func(ctx context.Context, addr string, timeout time.Duration) error {
	return func(ctx context.Context, addr string, timeout time.Duration) error {
		secs := int(timeout.Seconds())
		if secs < 1 {
			secs = 1
		}
		_, err := run(ctx, "-sS", "--connect-only", "--connect-timeout", strconv.Itoa(secs), "telnet://"+addr)
		return err
	}
}
