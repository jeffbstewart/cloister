package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSearcherEngineSwapSameShape drives Kagi and Brave against stub servers
// returning their real response shapes, and asserts both trim to the same Hit
// shape — the two-implementation proof that the Searcher seam is real.
func TestSearcherEngineSwapSameShape(t *testing.T) {
	kagi := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer k-key" {
			t.Errorf("kagi auth header = %q, want %q", got, "Bearer k-key")
		}
		w.Write([]byte(`{"data":{"search":[
			{"url":"https://a.example/1","title":"A","snippet":"sa"},
			{"title":"noURL","snippet":"skip"},
			{"url":"https://b.example/2","title":"B","snippet":"sb"}]}}`))
	}))
	defer kagi.Close()
	brave := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Subscription-Token"); got != "b-key" {
			t.Errorf("brave token header = %q, want %q", got, "b-key")
		}
		w.Write([]byte(`{"web":{"results":[
			{"title":"A","url":"https://a.example/1","description":"sa"},
			{"title":"B","url":"https://b.example/2","description":"sb"}]}}`))
	}))
	defer brave.Close()

	want := []Hit{
		{Title: "A", URL: "https://a.example/1", Snippet: "sa"},
		{Title: "B", URL: "https://b.example/2", Snippet: "sb"},
	}
	for _, s := range []Searcher{
		NewKagiSearcher(kagi.URL, "k-key", kagi.Client(), 1<<20),
		NewBraveSearcher(brave.URL, "b-key", brave.Client(), 1<<20),
	} {
		hits, err := s.Search(context.Background(), "q", 10)
		if err != nil {
			t.Fatalf("%s search: %v", s.Name(), err)
		}
		if len(hits) != 2 {
			t.Fatalf("%s: got %d hits, want 2 (t!=0 blocks skipped)", s.Name(), len(hits))
		}
		for i := range want {
			if hits[i] != want[i] {
				t.Errorf("%s hit %d = %+v, want %+v", s.Name(), i, hits[i], want[i])
			}
		}
	}
}

func TestCleanText(t *testing.T) {
	got := cleanText("Julius Caesar&#39;s <strong>Republic</strong> &amp; Rome")
	if want := "Julius Caesar's Republic & Rome"; got != want {
		t.Errorf("cleanText = %q, want %q", got, want)
	}
}

func TestSearcherCountClamp(t *testing.T) {
	var gotCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req kagiSearchReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotCount = req.Extract.Count
		w.Write([]byte(`{"data":{"search":[]}}`))
	}))
	defer srv.Close()
	s := NewKagiSearcher(srv.URL, "k", srv.Client(), 1<<20)
	if _, err := s.Search(context.Background(), "q", 999); err != nil {
		t.Fatal(err)
	}
	if gotCount != 10 {
		t.Errorf("count 999 sent extract.count=%d, want clamped to 10 (policy.MaxResultCount)", gotCount)
	}
}
