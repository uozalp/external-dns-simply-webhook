package simply

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFilterDomains(t *testing.T) {
	all := []Domain{
		{Object: "kvolbæk.dk", Name: "xn--kvolbk-tua.dk"},
		{Object: "example.com", Name: "example.com"},
	}

	found, unknown := FilterDomains(all, []string{"kvolbæk.dk", " xn--kvolbk-tua.dk", "EXAMPLE.com", "nope.dk"})

	if len(found) != 2 || found[0] != all[0] || found[1] != all[1] {
		t.Errorf("found = %v, want each domain once", found)
	}
	if len(unknown) != 1 || unknown[0] != "nope.dk" {
		t.Errorf("unknown = %v, want [nope.dk]", unknown)
	}
}

func TestListDomainsAndRecordPaths(t *testing.T) {
	var requests []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("unexpected Authorization header %q", r.Header.Get("Authorization"))
		}
		requests = append(requests, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/my/products/" {
			_ = json.NewEncoder(w).Encode(map[string]any{"products": []map[string]any{
				{"object": "kvolbæk.dk", "domain": map[string]any{"name": "kvolbæk.dk", "name_idn": "xn--kvolbk-tua.dk", "managed": true}},
				{"object": "example.com", "domain": map[string]any{"name": "example.com", "name_idn": "example.com", "managed": true}},
				{"object": "unmanaged.com", "domain": map[string]any{"name": "unmanaged.com", "name_idn": "unmanaged.com", "managed": false}},
			}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"records": []any{}})
	}))
	defer server.Close()

	client := NewClient("secret")
	client.BaseURL = server.URL + "/"

	domains, err := client.ListDomains()
	if err != nil {
		t.Fatalf("ListDomains: %v", err)
	}
	expected := []Domain{
		{Object: "kvolbæk.dk", Name: "xn--kvolbk-tua.dk"},
		{Object: "example.com", Name: "example.com"},
	}
	if len(domains) != len(expected) {
		t.Fatalf("ListDomains = %v, want %v", domains, expected)
	}
	for i := range expected {
		if domains[i] != expected[i] {
			t.Errorf("domain %d = %v, want %v", i, domains[i], expected[i])
		}
	}

	if _, err := client.ListRecords("kvolbæk.dk"); err != nil {
		t.Fatalf("ListRecords: %v", err)
	}
	if err := client.AddRecord("kvolbæk.dk", Record{Type: "A", Name: "www", Data: "192.0.2.1"}); err != nil {
		t.Fatalf("AddRecord: %v", err)
	}
	if err := client.UpdateRecord("kvolbæk.dk", Record{ID: 7, Type: "A", Name: "www", Data: "192.0.2.2"}); err != nil {
		t.Fatalf("UpdateRecord: %v", err)
	}
	if err := client.DeleteRecord("kvolbæk.dk", Record{ID: 7}); err != nil {
		t.Fatalf("DeleteRecord: %v", err)
	}

	expectedRequests := []string{
		"GET /my/products/",
		"GET /my/products/kvolbæk.dk/dns/records/",
		"POST /my/products/kvolbæk.dk/dns/records/",
		"PUT /my/products/kvolbæk.dk/dns/records/7/",
		"DELETE /my/products/kvolbæk.dk/dns/records/7/",
	}
	if len(requests) != len(expectedRequests) {
		t.Fatalf("requests = %q, want %q", requests, expectedRequests)
	}
	for i := range expectedRequests {
		if requests[i] != expectedRequests[i] {
			t.Errorf("request %d = %q, want %q", i, requests[i], expectedRequests[i])
		}
	}
}

func TestRetriesOnlySafeRequests(t *testing.T) {
	var requests []string
	failures := 1

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method)
		if failures > 0 {
			failures--
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"records": []any{}})
	}))
	defer server.Close()

	client := NewClient("secret")
	client.BaseURL = server.URL + "/"
	client.RetryDelay = time.Millisecond

	if _, err := client.ListRecords("example.com"); err != nil {
		t.Fatalf("ListRecords should succeed after one retry: %v", err)
	}
	if len(requests) != 2 {
		t.Errorf("GET requests = %v, want 2", requests)
	}

	requests, failures = nil, 1
	if err := client.AddRecord("example.com", Record{Type: "A", Name: "www", Data: "192.0.2.1"}); err == nil {
		t.Error("AddRecord should fail without a retry")
	}
	if len(requests) != 1 {
		t.Errorf("POST requests = %v, want 1", requests)
	}
}

func TestRetryWaitFollowsRetryAfter(t *testing.T) {
	failures := 2
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failures > 0 {
			failures--
			w.Header().Set("Retry-After", "3")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"records": []any{}})
	}))
	defer server.Close()

	var waits []time.Duration
	client := NewClient("secret")
	client.BaseURL = server.URL + "/"
	client.RetryDelay = time.Millisecond
	client.sleep = func(d time.Duration) { waits = append(waits, d) }

	if _, err := client.ListRecords("example.com"); err != nil {
		t.Fatalf("ListRecords: %v", err)
	}
	if len(waits) != 2 || waits[0] != 3*time.Second || waits[1] != 3*time.Second {
		t.Errorf("waits = %v, want [3s 3s]", waits)
	}
}

func TestRetryWaitIsCapped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600")
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	defer server.Close()

	var waits []time.Duration
	client := NewClient("secret")
	client.BaseURL = server.URL + "/"
	client.sleep = func(d time.Duration) { waits = append(waits, d) }

	if _, err := client.ListRecords("example.com"); err == nil {
		t.Fatal("ListRecords should fail after the last attempt")
	}
	if len(waits) != maxAttempts-1 || waits[0] != maxRetryWait {
		t.Errorf("waits = %v, want %d waits of %v", waits, maxAttempts-1, maxRetryWait)
	}
}

func TestRetriedDeleteTreatsNotFoundAsDone(t *testing.T) {
	var statuses []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status := http.StatusNotFound
		if len(statuses) == 0 {
			status = http.StatusServiceUnavailable
		}
		statuses = append(statuses, status)
		http.Error(w, "x", status)
	}))
	defer server.Close()

	client := NewClient("secret")
	client.BaseURL = server.URL + "/"
	client.RetryDelay = time.Millisecond

	if err := client.DeleteRecord("example.com", Record{ID: 7}); err != nil {
		t.Errorf("DeleteRecord after a retry should treat 404 as done: %v", err)
	}
	if len(statuses) != 2 {
		t.Errorf("statuses = %v, want a 503 then a 404", statuses)
	}
}

func TestDeleteNotFoundOnFirstTryIsAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "missing", http.StatusNotFound)
	}))
	defer server.Close()

	client := NewClient("secret")
	client.BaseURL = server.URL + "/"

	if err := client.DeleteRecord("example.com", Record{ID: 7}); err == nil {
		t.Error("a 404 on the first DELETE should be reported")
	}
}

func TestParseRetryAfter(t *testing.T) {
	if got := parseRetryAfter("5"); got != 5*time.Second {
		t.Errorf("seconds form = %v", got)
	}
	if got := parseRetryAfter(time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)); got < 59*time.Minute {
		t.Errorf("date form = %v", got)
	}
	for _, bad := range []string{"", "soon", "-3"} {
		if got := parseRetryAfter(bad); got != 0 {
			t.Errorf("parseRetryAfter(%q) = %v, want 0", bad, got)
		}
	}
}
