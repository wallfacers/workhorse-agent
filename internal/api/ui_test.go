package api

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestUI_ServesIndex(t *testing.T) {
	_, ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/ui/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(body), "<title>workhorse-agent</title>") {
		t.Fatalf("body missing page title; got: %s", truncForLog(string(body)))
	}
}

func TestUI_ServesAppJS(t *testing.T) {
	_, ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/ui/app.js")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "EventSource") {
		t.Fatalf("app.js missing EventSource; got: %s", truncForLog(string(body)))
	}
}

func TestUI_RedirectsBareUI(t *testing.T) {
	_, ts := newTestServer(t)
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(ts.URL + "/ui")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Fatalf("want 301, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/ui/" {
		t.Fatalf("want Location=/ui/, got %q", loc)
	}
}

func truncForLog(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}
