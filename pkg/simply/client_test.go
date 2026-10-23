package simply

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListDomainsAndRecordPaths(t *testing.T) {
	var requests []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("unexpected Authorization header %q", r.Header.Get("Authorization"))
		}
		requests = append(requests, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/my/products/" {
			json.NewEncoder(w).Encode(map[string]any{"products": []map[string]any{
				{"object": "kvolbæk.dk", "domain": map[string]any{"name": "kvolbæk.dk", "name_idn": "xn--kvolbk-tua.dk", "managed": true}},
				{"object": "example.com", "domain": map[string]any{"name": "example.com", "name_idn": "example.com", "managed": true}},
				{"object": "unmanaged.com", "domain": map[string]any{"name": "unmanaged.com", "name_idn": "unmanaged.com", "managed": false}},
			}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"records": []any{}})
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
