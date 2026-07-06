package wire

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// client returns an http.Client that can reach the test server directly (the
// guarded client is exercised in transport_test.go; here we test the capped
// request helpers).
func client() *http.Client { return &http.Client{Timeout: 5 * time.Second} }

func TestGetBytesReturnsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("hello"))
	}))
	defer srv.Close()
	b, err := GetBytes(context.Background(), client(), srv.URL, nil, 1024)
	if err != nil || string(b) != "hello" {
		t.Fatalf("GetBytes = %q, %v", b, err)
	}
}

func TestOverCapIsErrResponseTooBig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(make([]byte, 100))
	}))
	defer srv.Close()
	_, err := GetBytes(context.Background(), client(), srv.URL, nil, 99)
	if !errors.Is(err, ErrResponseTooBig) {
		t.Fatalf("err = %v, want ErrResponseTooBig", err)
	}
	// Exactly at the cap is fine.
	if _, err := GetBytes(context.Background(), client(), srv.URL, nil, 100); err != nil {
		t.Fatalf("at-cap err = %v", err)
	}
}

// TestNon2xxErrorCarriesBodySnippet: the error includes the upstream body —
// the documented reason every caller must scrub it before surfacing.
func TestNon2xxErrorCarriesBodySnippet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad token sekret", http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, err := GetBytes(context.Background(), client(), srv.URL, nil, 1024)
	if err == nil || !strings.Contains(err.Error(), "sekret") {
		t.Fatalf("err = %v, want the upstream body snippet included", err)
	}
	if got := NewScrubber("sekret").Scrub(err.Error()); strings.Contains(got, "sekret") {
		t.Fatalf("scrubbed error still leaks: %q", got)
	}
}

func TestPostJSONSendsBodyAndContentType(t *testing.T) {
	var gotCT, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		b := make([]byte, r.ContentLength)
		r.Body.Read(b)
		gotBody = string(b)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	_, err := PostJSON(context.Background(), client(), srv.URL, http.Header{"X-Auth": {"k"}}, []byte(`{"q":1}`), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if gotCT != "application/json" || gotBody != `{"q":1}` {
		t.Errorf("content-type %q body %q", gotCT, gotBody)
	}
}
