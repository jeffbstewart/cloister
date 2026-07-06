package wire

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// GetBytes performs one GET through the guarded client; see doCapped for the
// response contract.
func GetBytes(ctx context.Context, hc *http.Client, rawURL string, header http.Header, maxBytes int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	setHeader(req, header)
	return doCapped(hc, req, maxBytes)
}

// PostJSON performs one POST of a JSON body through the guarded client (Kagi's
// extract endpoint is POST).  Same response contract as GetBytes.
func PostJSON(ctx context.Context, hc *http.Client, rawURL string, header http.Header, body []byte, maxBytes int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	setHeader(req, header)
	return doCapped(hc, req, maxBytes)
}

func setHeader(req *http.Request, header http.Header) {
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
}

// doCapped runs the request and returns the response body, capped at maxBytes.  A
// non-2xx status is an error that INCLUDES a snippet of the upstream body —
// which, for a provider 4xx/5xx, may echo our auth header — so every caller runs
// the result through the scrubber before it leaves the egress subsystem.  An
// over-cap body is ErrResponseTooBig.
func doCapped(hc *http.Client, req *http.Request, maxBytes int64) ([]byte, error) {
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, ErrResponseTooBig
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("upstream %s: %s: %s", req.URL.Host, resp.Status, strings.TrimSpace(string(body)))
	}
	return body, nil
}
