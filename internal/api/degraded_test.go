package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

// A degraded server (e.g. started without a usable provider key) stays reachable
// on /health but reports ok:false with a machine-readable reason, so a managed
// launcher can attach and guide the user instead of facing a crash-loop.
func TestHealth_DegradedReason(t *testing.T) {
	_, ts := newTestServer(t, func(c *Config) {
		c.DegradedReason = "no_provider_key"
	})
	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("get health: %v", err)
	}
	defer resp.Body.Close()
	// Availability is carried by `ok`, not the HTTP status — probes still see 200.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["ok"] != false {
		t.Fatalf("ok: want false, got %v", body["ok"])
	}
	if body["reason"] != "no_provider_key" {
		t.Fatalf("reason: want no_provider_key, got %v", body["reason"])
	}
}

// A healthy server omits `reason` entirely (backward-compatible shape).
func TestHealth_HealthyOmitsReason(t *testing.T) {
	_, ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("get health: %v", err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["ok"] != true {
		t.Fatalf("ok: want true, got %v", body["ok"])
	}
	if _, present := body["reason"]; present {
		t.Fatalf("reason should be absent when healthy, got %v", body["reason"])
	}
}

// Session creation is rejected while degraded: a session would be un-runnable,
// so the launcher gets the machine-readable reason as the error code.
func TestCreateSession_DegradedRejected(t *testing.T) {
	_, ts := newTestServer(t, func(c *Config) {
		c.DegradedReason = "no_provider_key"
	})
	reqBody, _ := json.Marshal(map[string]any{
		"workdir":  "/tmp/proj",
		"provider": "anthropic",
		"model":    "claude-sonnet-4-6",
	})
	resp, err := http.Post(ts.URL+"/v1/sessions", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status: want 503, got %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["code"] != "no_provider_key" {
		t.Fatalf("code: want no_provider_key, got %v", body["code"])
	}
}
