package ledger

import (
	"net/http"
	"time"
)

// retryTransport retries idempotent requests (GET/HEAD) on connection errors
// (e.g. "unexpected EOF" under load) and 502/503/504, up to 3 attempts with
// backoff. Writes are never retried: a timed-out POST may have succeeded.
type retryTransport struct {
	base  http.RoundTripper
	sleep func(time.Duration)
}

func (t retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return t.base.RoundTrip(req)
	}
	var resp *http.Response
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			t.sleep(time.Duration(attempt) * time.Second)
		}
		resp, err = t.base.RoundTrip(req)
		if err == nil && resp.StatusCode != http.StatusBadGateway &&
			resp.StatusCode != http.StatusServiceUnavailable && resp.StatusCode != http.StatusGatewayTimeout {
			return resp, nil
		}
		if req.Context().Err() != nil {
			break
		}
		if err == nil && attempt < 2 {
			resp.Body.Close()
		}
	}
	return resp, err
}

func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: retryTransport{base: http.DefaultTransport, sleep: time.Sleep},
	}
}
