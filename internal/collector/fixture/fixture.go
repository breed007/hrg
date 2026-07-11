// Package fixture replays recorded API responses from a directory, so every
// collector can run — in tests, demos, and development — without live
// infrastructure. The recorded files double as documentation of each API's
// shape.
//
// A request is mapped to a file by its URL path: strip the leading slash,
// replace the remaining slashes with underscores, append ".json". So
// GET /api2/json/cluster/resources is served from
// api2_json_cluster_resources.json, and GET /containers/json?all=1 from
// containers_json.json (query strings are ignored).
package fixture

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Transport is an http.RoundTripper serving responses from Dir.
type Transport struct {
	Dir string
}

// FileForPath returns the fixture filename a URL path maps to.
func FileForPath(urlPath string) string {
	return strings.ReplaceAll(strings.Trim(urlPath, "/"), "/", "_") + ".json"
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	name := FileForPath(req.URL.Path)
	body, err := os.ReadFile(filepath.Join(t.Dir, name))
	if err != nil {
		return nil, fmt.Errorf("fixture %s (for %s %s): %w", name, req.Method, req.URL.Path, err)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
		Request:    req,
	}, nil
}

// Client returns an *http.Client that serves everything from dir.
func Client(dir string) *http.Client {
	return &http.Client{Transport: &Transport{Dir: dir}}
}
