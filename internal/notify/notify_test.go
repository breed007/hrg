package notify

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSend(t *testing.T) {
	var gotTitle, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTitle = r.Header.Get("Title")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
	}))
	defer srv.Close()

	if err := Send(context.Background(), srv.URL, "HRG drift", "2 changed"); err != nil {
		t.Fatal(err)
	}
	if gotTitle != "HRG drift" || gotBody != "2 changed" {
		t.Errorf("got title=%q body=%q", gotTitle, gotBody)
	}

	// Empty URL is a silent no-op.
	if err := Send(context.Background(), "", "x", "y"); err != nil {
		t.Errorf("empty url should be no-op: %v", err)
	}

	// Server errors surface.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer bad.Close()
	if err := Send(context.Background(), bad.URL, "x", "y"); err == nil {
		t.Error("HTTP error should surface")
	}
}
