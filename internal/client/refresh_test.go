package client

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aloisdeniel/cairn/internal/auth"
)

func TestClientPicksUpRenewedToken(t *testing.T) {
	var seen []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") == "Bearer old" {
			w.Header().Set(auth.RefreshHeader, "new")
		}
		w.Write([]byte(`{}`))
	}))
	defer ts.Close()

	c := New(ts.URL, "old")
	var saved []string
	c.OnToken = func(tok string) { saved = append(saved, tok) }
	for range 2 {
		if _, err := c.Me(); err != nil {
			t.Fatal(err)
		}
	}
	if c.Token != "new" || len(saved) != 1 || saved[0] != "new" {
		t.Fatalf("token %q, saved %v", c.Token, saved)
	}
	if seen[1] != "Bearer new" {
		t.Fatalf("second request sent %q", seen[1])
	}
}
