package webhook

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/uozalp/external-dns-simply-webhook/pkg/simply"
)

func newTestHandler(t *testing.T, requests *[]string) *Handler {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*requests = append(*requests, r.Method+" "+r.URL.Path+" "+strings.TrimSpace(string(body)))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/my/products/kvolbæk.dk/dns/records/":
			json.NewEncoder(w).Encode(map[string]any{"records": []map[string]any{
				{"record_id": 1, "name": "@", "type": "A", "data": "192.0.2.1", "ttl": 3600},
				{"record_id": 2, "name": "www", "type": "A", "data": "192.0.2.2", "ttl": 3600},
			}})
		case "/my/products/www.example.com/dns/records/":
			json.NewEncoder(w).Encode(map[string]any{"records": []map[string]any{
				{"record_id": 3, "name": "app", "type": "A", "data": "192.0.2.3", "ttl": 3600},
			}})
		default:
			json.NewEncoder(w).Encode(map[string]any{"records": []any{}})
		}
	}))
	t.Cleanup(server.Close)

	client := simply.NewClient("secret")
	client.BaseURL = server.URL + "/"

	return NewHandler(client, slog.New(slog.NewTextHandler(io.Discard, nil)), []simply.Domain{
		{Object: "kvolbæk.dk", Name: "xn--kvolbk-tua.dk"},
		{Object: "example.com", Name: "example.com"},
		{Object: "www.example.com", Name: "www.example.com"},
	})
}

func TestGetRecordsUsesPunycodeNames(t *testing.T) {
	var requests []string
	handler := newTestHandler(t, &requests)

	recorder := httptest.NewRecorder()
	handler.GetRecords(recorder, httptest.NewRequest(http.MethodGet, "/records", nil))

	var endpoints []struct {
		DNSName string `json:"dnsName"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&endpoints); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	var names []string
	for _, ep := range endpoints {
		names = append(names, ep.DNSName)
	}
	expected := "xn--kvolbk-tua.dk www.xn--kvolbk-tua.dk app.www.example.com"
	if strings.Join(names, " ") != expected {
		t.Errorf("dnsNames = %q, want %q", strings.Join(names, " "), expected)
	}
}

func TestApplyChangesResolvesProductObject(t *testing.T) {
	var requests []string
	handler := newTestHandler(t, &requests)

	changes := `{
		"create": [
			{"dnsName": "new.xn--kvolbk-tua.dk", "recordType": "A", "targets": ["192.0.2.10"]},
			{"dnsName": "api.www.example.com", "recordType": "A", "targets": ["192.0.2.11"]},
			{"dnsName": "api.example.com", "recordType": "A", "targets": ["192.0.2.12"]}
		],
		"updateOld": [{"dnsName": "www.xn--kvolbk-tua.dk", "recordType": "A", "targets": ["192.0.2.2"]}],
		"updateNew": [{"dnsName": "www.xn--kvolbk-tua.dk", "recordType": "A", "targets": ["192.0.2.20"]}],
		"delete": [{"dnsName": "app.www.example.com", "recordType": "A", "targets": ["192.0.2.3"]}]
	}`
	recorder := httptest.NewRecorder()
	handler.ApplyChanges(recorder, httptest.NewRequest(http.MethodPost, "/records", strings.NewReader(changes)))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body %q", recorder.Code, recorder.Body.String())
	}

	var writes []string
	for _, request := range requests {
		if !strings.HasPrefix(request, "GET ") {
			fields := strings.SplitN(request, " ", 3)
			writes = append(writes, fields[0]+" "+fields[1])
		}
	}
	expected := []string{
		"POST /my/products/kvolbæk.dk/dns/records/",
		"POST /my/products/www.example.com/dns/records/",
		"POST /my/products/example.com/dns/records/",
		"PUT /my/products/kvolbæk.dk/dns/records/2/",
		"DELETE /my/products/www.example.com/dns/records/3/",
	}
	if strings.Join(writes, "\n") != strings.Join(expected, "\n") {
		t.Errorf("writes:\n%s\nwant:\n%s", strings.Join(writes, "\n"), strings.Join(expected, "\n"))
	}
}

func TestExtractDomainUnknown(t *testing.T) {
	var requests []string
	handler := newTestHandler(t, &requests)

	if _, err := handler.extractDomain("www.unknown.dk"); err == nil {
		t.Error("expected error for a name outside the managed domains")
	}
	object, err := handler.extractDomain("WWW.KVOLBÆK.DK.")
	if err != nil || object != "kvolbæk.dk" {
		t.Errorf("extractDomain = %q, %v", object, err)
	}
}

func TestApplyChangesKeepsRecordIDsPerProduct(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests = append(requests, r.Method+" "+r.URL.Path+" "+strings.TrimSpace(string(body)))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/my/products/example.com/dns/records/":
			json.NewEncoder(w).Encode(map[string]any{"records": []map[string]any{
				{"record_id": 10, "name": "www", "type": "A", "data": "192.0.2.10", "ttl": 3600},
			}})
		case "/my/products/www.example.com/dns/records/":
			json.NewEncoder(w).Encode(map[string]any{"records": []map[string]any{
				{"record_id": 20, "name": "@", "type": "A", "data": "192.0.2.20", "ttl": 3600},
			}})
		default:
			json.NewEncoder(w).Encode(map[string]any{"records": []any{}})
		}
	}))
	defer server.Close()

	client := simply.NewClient("secret")
	client.BaseURL = server.URL + "/"
	handler := NewHandler(client, slog.New(slog.NewTextHandler(io.Discard, nil)), []simply.Domain{
		{Object: "www.example.com", Name: "www.example.com"},
		{Object: "example.com", Name: "example.com"},
	})

	changes := `{
		"updateOld": [{"dnsName": "www.example.com", "recordType": "A", "targets": ["192.0.2.20"]}],
		"updateNew": [{"dnsName": "www.example.com", "recordType": "A", "targets": ["192.0.2.21"]}],
		"delete": [{"dnsName": "www.example.com", "recordType": "A", "targets": ["192.0.2.21"]}]
	}`
	recorder := httptest.NewRecorder()
	handler.ApplyChanges(recorder, httptest.NewRequest(http.MethodPost, "/records", strings.NewReader(changes)))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body.String())
	}

	var writes []string
	for _, request := range requests {
		if !strings.HasPrefix(request, http.MethodGet+" ") {
			writes = append(writes, strings.SplitN(request, " ", 3)[0]+" "+strings.SplitN(request, " ", 3)[1])
		}
	}
	expected := "PUT /my/products/www.example.com/dns/records/20/ DELETE /my/products/www.example.com/dns/records/20/"
	if strings.Join(writes, " ") != expected {
		t.Errorf("writes = %q, want %q", strings.Join(writes, " "), expected)
	}
}
