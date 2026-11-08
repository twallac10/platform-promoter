package ledger

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type scripted struct {
	codes []int // 0 = connection error
	calls int
}

func (s *scripted) RoundTrip(*http.Request) (*http.Response, error) {
	c := s.codes[min(s.calls, len(s.codes)-1)]
	s.calls++
	if c == 0 {
		return nil, errors.New("unexpected EOF")
	}
	return &http.Response{StatusCode: c, Body: io.NopCloser(strings.NewReader(""))}, nil
}

func do(t *testing.T, method string, codes ...int) (int, *scripted, error) {
	s := &scripted{codes: codes}
	c := &http.Client{Transport: retryTransport{base: s, sleep: func(time.Duration) {}}}
	req, _ := http.NewRequest(method, "https://api.github.com/x", nil)
	resp, err := c.Do(req)
	if err != nil {
		return 0, s, err
	}
	return resp.StatusCode, s, nil
}

func TestRetryTransport(t *testing.T) {
	if code, s, err := do(t, "GET", 0, 502, 200); err != nil || code != 200 || s.calls != 3 {
		t.Fatalf("GET should retry EOF and 502: code=%d calls=%d err=%v", code, s.calls, err)
	}
	if _, s, err := do(t, "GET", 0, 0, 0); err == nil || s.calls != 3 {
		t.Fatalf("GET should give up after 3 attempts: calls=%d err=%v", s.calls, err)
	}
	if code, s, _ := do(t, "GET", 404); code != 404 || s.calls != 1 {
		t.Fatalf("404 is an answer, not retried: calls=%d", s.calls)
	}
	if _, s, _ := do(t, "POST", 0, 200); s.calls != 1 {
		t.Fatalf("POST must not be retried: calls=%d", s.calls)
	}
}
