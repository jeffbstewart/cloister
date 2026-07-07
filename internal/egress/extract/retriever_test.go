// Copyright 2026 Jeffrey B. Stewart
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package extract

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestKagiRetrieverPostsAndReturnsMarkdown(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotRaw string
	var gotBody extractRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotRaw = string(b)
		json.Unmarshal(b, &gotBody)
		// format:markdown returns the extracted markdown DIRECTLY (not JSON).
		w.Write([]byte("## Extraction Results\n\n### URL: https://a.example/1\n\n# A\nbody"))
	}))
	defer srv.Close()

	r := NewKagiRetriever(srv.URL, "k-key", srv.Client(), 1<<20, 20*time.Second)
	ext, err := r.Fetch(context.Background(), "https://a.example/1")
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/api/v1/extract" {
		t.Errorf("path = %s, want /api/v1/extract", gotPath)
	}
	if gotAuth != "Bearer k-key" {
		t.Errorf("auth = %q, want %q (extract uses Bearer, not Bot)", gotAuth, "Bearer k-key")
	}
	if len(gotBody.Pages) != 1 || gotBody.Pages[0].URL != "https://a.example/1" {
		t.Errorf("request pages = %+v", gotBody.Pages)
	}
	// A time.Duration in memory, floating-point seconds (one decimal) on the wire.
	if gotBody.Timeout != timeoutSeconds(20*time.Second) {
		t.Errorf("timeout round-trip = %v, want 20s", time.Duration(gotBody.Timeout))
	}
	if !strings.Contains(gotRaw, `"timeout":20.0`) {
		t.Errorf("wire body = %s, want \"timeout\":20.0", gotRaw)
	}
	if !strings.Contains(ext.Markdown, "# A\nbody") {
		t.Errorf("markdown = %q, want it to contain the page body", ext.Markdown)
	}
	if ext.FinalURL != "https://a.example/1" {
		t.Errorf("finalURL = %q, want the requested URL", ext.FinalURL)
	}
}

func TestKagiRetrieverSurfacesError(t *testing.T) {
	// A failed fetch comes back as a non-2xx; wire.PostJSON surfaces the body in the error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "fetch timed out", http.StatusBadGateway)
	}))
	defer srv.Close()
	r := NewKagiRetriever(srv.URL, "k", srv.Client(), 1<<20, 20*time.Second)
	if _, err := r.Fetch(context.Background(), "https://a.example/1"); err == nil || !strings.Contains(err.Error(), "fetch timed out") {
		t.Errorf("err = %v, want the upstream error surfaced", err)
	}
}
