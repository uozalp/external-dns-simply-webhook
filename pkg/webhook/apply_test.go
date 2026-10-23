package webhook

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uozalp/external-dns-simply-webhook/pkg/simply"
)

type fakeSimply struct {
	mu       sync.Mutex
	requests []string
}

// writes returns the non-GET requests as "METHOD path body".
func (f *fakeSimply) writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var writes []string
	for _, request := range f.requests {
		if !strings.HasPrefix(request, "GET ") {
			writes = append(writes, strings.TrimSpace(request))
		}
	}
	return writes
}

// newFakeHandler serves the given records per product object; a product with
// no entry answers 500 on list.
func newFakeHandler(t *testing.T, domains []simply.Domain, records map[string][]map[string]any) (*Handler, *fakeSimply) {
	t.Helper()

	fake := &fakeSimply{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fake.mu.Lock()
		fake.requests = append(fake.requests, r.Method+" "+r.URL.Path+" "+strings.TrimSpace(string(body)))
		fake.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		object := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/my/products/"), "/dns/records/")
		list, ok := records[object]
		if r.Method == http.MethodGet && !ok {
			http.Error(w, "boom", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"records": list})
	}))
	t.Cleanup(server.Close)

	client := simply.NewClient("secret")
	client.BaseURL = server.URL + "/"
	return NewHandler(client, slog.New(slog.NewTextHandler(io.Discard, nil)), domains), fake
}

var exampleDomain = []simply.Domain{{Object: "example.com", Name: "example.com"}}

func record(id int, name, typ, data string, ttl int) map[string]any {
	return map[string]any{"record_id": id, "name": name, "type": typ, "data": data, "ttl": ttl}
}

func post(t *testing.T, handler http.HandlerFunc, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler(recorder, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
	return recorder
}

func checkWrites(t *testing.T, fake *fakeSimply, want ...string) {
	t.Helper()
	got := fake.writes()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("writes:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestGetRecordsGroupsValuesOfOneName(t *testing.T) {
	handler, _ := newFakeHandler(t, exampleDomain, map[string][]map[string]any{
		"example.com": {
			record(1, "www", "A", "192.0.2.1", 300),
			record(2, "www", "A", "192.0.2.2", 300),
			record(3, "www", "TXT", "hello", 300),
		},
	})

	recorder := httptest.NewRecorder()
	handler.GetRecords(recorder, httptest.NewRequest(http.MethodGet, "/records", nil))

	var endpoints []endpoint
	if err := json.NewDecoder(recorder.Body).Decode(&endpoints); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(endpoints) != 2 {
		t.Fatalf("endpoints = %+v, want 2", endpoints)
	}
	if got := strings.Join(endpoints[0].Targets, ","); endpoints[0].RecordType != "A" || got != "192.0.2.1,192.0.2.2" {
		t.Errorf("A endpoint = %+v", endpoints[0])
	}
}

func TestGetRecordsFailsWhenADomainFails(t *testing.T) {
	handler, _ := newFakeHandler(t,
		[]simply.Domain{{Object: "example.com", Name: "example.com"}, {Object: "broken.com", Name: "broken.com"}},
		map[string][]map[string]any{"example.com": {record(1, "www", "A", "192.0.2.1", 300)}},
	)

	recorder := httptest.NewRecorder()
	handler.GetRecords(recorder, httptest.NewRequest(http.MethodGet, "/records", nil))

	if recorder.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 instead of a partial list", recorder.Code)
	}
}

func TestUpdateAddsNewTarget(t *testing.T) {
	handler, fake := newFakeHandler(t, exampleDomain, map[string][]map[string]any{
		"example.com": {record(1, "www", "A", "192.0.2.1", 300)},
	})

	body := `{"updateOld": [{"dnsName": "www.example.com", "recordType": "A", "targets": ["192.0.2.1"]}],
		"updateNew": [{"dnsName": "www.example.com", "recordType": "A", "recordTTL": 300, "targets": ["192.0.2.1", "192.0.2.2"]}]}`
	if recorder := post(t, handler.ApplyChanges, body); recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body %q", recorder.Code, recorder.Body.String())
	}

	checkWrites(t, fake, `POST /my/products/example.com/dns/records/ {"comment":"Managed by External-DNS","data":"192.0.2.2","name":"www.example.com","ttl":300,"type":"A"}`)
}

func TestUpdateRewritesStaleAndRemovesExtraRecords(t *testing.T) {
	handler, fake := newFakeHandler(t, exampleDomain, map[string][]map[string]any{
		"example.com": {
			record(1, "www", "A", "192.0.2.1", 300),
			record(2, "www", "A", "192.0.2.2", 300),
			record(3, "www", "A", "192.0.2.3", 300),
		},
	})

	body := `{"updateNew": [{"dnsName": "www.example.com", "recordType": "A", "recordTTL": 300, "targets": ["192.0.2.1", "192.0.2.9"]}]}`
	if recorder := post(t, handler.ApplyChanges, body); recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body %q", recorder.Code, recorder.Body.String())
	}

	checkWrites(t, fake,
		`PUT /my/products/example.com/dns/records/2/ {"comment":"Managed by External-DNS","data":"192.0.2.9","name":"www.example.com","ttl":300,"type":"A"}`,
		`DELETE /my/products/example.com/dns/records/3/`,
	)
}

func TestUpdateTTL(t *testing.T) {
	records := map[string][]map[string]any{"example.com": {record(1, "www", "A", "192.0.2.1", 300)}}

	handler, fake := newFakeHandler(t, exampleDomain, records)
	body := `{"updateNew": [{"dnsName": "www.example.com", "recordType": "A", "recordTTL": 60, "targets": ["192.0.2.1"]}]}`
	if recorder := post(t, handler.ApplyChanges, body); recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d", recorder.Code)
	}
	checkWrites(t, fake, `PUT /my/products/example.com/dns/records/1/ {"comment":"Managed by External-DNS","data":"192.0.2.1","name":"www.example.com","ttl":60,"type":"A"}`)

	handler, fake = newFakeHandler(t, exampleDomain, records)
	body = `{"updateNew": [{"dnsName": "www.example.com", "recordType": "A", "targets": ["192.0.2.1"]}]}`
	if recorder := post(t, handler.ApplyChanges, body); recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d", recorder.Code)
	}
	checkWrites(t, fake)
}

func TestUpdateWithoutOldEndpointsOrTargetsDoesNotPanic(t *testing.T) {
	handler, _ := newFakeHandler(t, exampleDomain, map[string][]map[string]any{"example.com": {}})

	body := `{"updateOld": [], "updateNew": [{"dnsName": "www.example.com", "recordType": "A", "targets": ["192.0.2.1"]}]}`
	if recorder := post(t, handler.ApplyChanges, body); recorder.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", recorder.Code)
	}

	body = `{"updateNew": [{"dnsName": "www.example.com", "recordType": "A", "targets": []}]}`
	if recorder := post(t, handler.ApplyChanges, body); recorder.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 for an update without targets", recorder.Code)
	}
}

func TestDeleteRemovesEveryMatchingValue(t *testing.T) {
	handler, fake := newFakeHandler(t, exampleDomain, map[string][]map[string]any{
		"example.com": {
			record(1, "www", "A", "192.0.2.1", 300),
			record(2, "www", "A", "192.0.2.2", 300),
			record(3, "www", "A", "192.0.2.3", 300),
			record(4, "txt", "TXT", "heritage=external-dns", 300),
		},
	})

	body := `{"delete": [
		{"dnsName": "www.example.com", "recordType": "A", "targets": ["192.0.2.1", "192.0.2.2"]},
		{"dnsName": "txt.example.com", "recordType": "TXT", "targets": ["\"heritage=external-dns\""]}
	]}`
	if recorder := post(t, handler.ApplyChanges, body); recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body %q", recorder.Code, recorder.Body.String())
	}

	checkWrites(t, fake,
		`DELETE /my/products/example.com/dns/records/1/`,
		`DELETE /my/products/example.com/dns/records/2/`,
		`DELETE /my/products/example.com/dns/records/4/`,
	)
}

func TestDeleteWithoutTargetsRemovesAllValues(t *testing.T) {
	handler, fake := newFakeHandler(t, exampleDomain, map[string][]map[string]any{
		"example.com": {
			record(1, "www", "A", "192.0.2.1", 300),
			record(2, "www", "A", "192.0.2.2", 300),
		},
	})

	body := `{"delete": [{"dnsName": "www.example.com", "recordType": "A"}]}`
	if recorder := post(t, handler.ApplyChanges, body); recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d", recorder.Code)
	}

	checkWrites(t, fake,
		`DELETE /my/products/example.com/dns/records/1/`,
		`DELETE /my/products/example.com/dns/records/2/`,
	)
}

func TestCreateSkipsExistingRecords(t *testing.T) {
	handler, fake := newFakeHandler(t, exampleDomain, map[string][]map[string]any{
		"example.com": {record(1, "www", "A", "192.0.2.1", 300)},
	})

	body := `{"create": [{"dnsName": "www.example.com", "recordType": "A", "targets": ["192.0.2.1", "192.0.2.2"]}]}`
	if recorder := post(t, handler.ApplyChanges, body); recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d", recorder.Code)
	}

	checkWrites(t, fake, `POST /my/products/example.com/dns/records/ {"comment":"Managed by External-DNS","data":"192.0.2.2","name":"www.example.com","ttl":3600,"type":"A"}`)
}

func TestApplyChangesOnlyListsTouchedDomains(t *testing.T) {
	handler, fake := newFakeHandler(t,
		[]simply.Domain{{Object: "example.com", Name: "example.com"}, {Object: "other.com", Name: "other.com"}},
		map[string][]map[string]any{"example.com": {}, "other.com": {}},
	)

	body := `{"create": [{"dnsName": "www.example.com", "recordType": "A", "targets": ["192.0.2.1"]}]}`
	if recorder := post(t, handler.ApplyChanges, body); recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d", recorder.Code)
	}

	for _, request := range fake.requests {
		if strings.Contains(request, "other.com") {
			t.Errorf("unexpected request %q", request)
		}
	}
}

func TestApplyChangesRejectsBadBody(t *testing.T) {
	handler, _ := newFakeHandler(t, exampleDomain, map[string][]map[string]any{"example.com": {}})

	if recorder := post(t, handler.ApplyChanges, `{not json`); recorder.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", recorder.Code)
	}
}

func TestAdjustEndpointsNormalizesNamesAndKeepsOtherFields(t *testing.T) {
	handler, _ := newFakeHandler(t, exampleDomain, map[string][]map[string]any{"example.com": {}})

	body := `[{"dnsName": "WWW.Example.com.", "targets": ["192.0.2.1"], "recordType": "A", "recordTTL": 300,
		"labels": {"owner": "me"}, "providerSpecific": [{"name": "x", "value": "y"}]}]`
	recorder := post(t, handler.AdjustEndpoints, body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}

	var endpoints []map[string]any
	if err := json.NewDecoder(recorder.Body).Decode(&endpoints); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ep := endpoints[0]
	if ep["dnsName"] != "www.example.com" || ep["recordTTL"] != float64(300) || ep["labels"] == nil || ep["providerSpecific"] == nil {
		t.Errorf("endpoint = %v", ep)
	}
}

func mxRecord(id int, name, data string, priority, ttl int) map[string]any {
	r := record(id, name, "MX", data, ttl)
	r["priority"] = priority
	return r
}

func TestMXPriorityIsPartOfTheTarget(t *testing.T) {
	existing := map[string][]map[string]any{"example.com": {mxRecord(1, "@", "mail.example.com", 10, 300)}}

	handler, _ := newFakeHandler(t, exampleDomain, existing)
	recorder := httptest.NewRecorder()
	handler.GetRecords(recorder, httptest.NewRequest(http.MethodGet, "/records", nil))
	var endpoints []endpoint
	if err := json.NewDecoder(recorder.Body).Decode(&endpoints); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(endpoints) != 1 || strings.Join(endpoints[0].Targets, ",") != "10 mail.example.com" {
		t.Errorf("endpoints = %+v, want target %q", endpoints, "10 mail.example.com")
	}

	handler, fake := newFakeHandler(t, exampleDomain, existing)
	body := `{"create": [{"dnsName": "example.com", "recordType": "MX", "recordTTL": 300, "targets": ["20 mail2.example.com"]}]}`
	if recorder := post(t, handler.ApplyChanges, body); recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body %q", recorder.Code, recorder.Body.String())
	}
	checkWrites(t, fake, `POST /my/products/example.com/dns/records/ {"comment":"Managed by External-DNS","data":"mail2.example.com","name":"example.com","priority":20,"ttl":300,"type":"MX"}`)
}

func TestMXPriorityIsKeptWhenOnlyTTLChanges(t *testing.T) {
	handler, fake := newFakeHandler(t, exampleDomain, map[string][]map[string]any{
		"example.com": {mxRecord(1, "@", "mail.example.com", 10, 300)},
	})

	body := `{"updateNew": [{"dnsName": "example.com", "recordType": "MX", "recordTTL": 60, "targets": ["10 mail.example.com"]}]}`
	if recorder := post(t, handler.ApplyChanges, body); recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body %q", recorder.Code, recorder.Body.String())
	}
	checkWrites(t, fake, `PUT /my/products/example.com/dns/records/1/ {"comment":"Managed by External-DNS","data":"mail.example.com","name":"example.com","priority":10,"ttl":60,"type":"MX"}`)
}

func TestMXTargetWithoutPriorityIsRejected(t *testing.T) {
	handler, fake := newFakeHandler(t, exampleDomain, map[string][]map[string]any{"example.com": {}})

	body := `{"create": [{"dnsName": "example.com", "recordType": "MX", "targets": ["mail.example.com"]}]}`
	if recorder := post(t, handler.ApplyChanges, body); recorder.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", recorder.Code)
	}
	checkWrites(t, fake)
}

func TestCreateWritesRepeatedTargetOnce(t *testing.T) {
	handler, fake := newFakeHandler(t, exampleDomain, map[string][]map[string]any{"example.com": {}})

	body := `{"create": [{"dnsName": "www.example.com", "recordType": "A", "targets": ["192.0.2.1", "192.0.2.1"]}]}`
	if recorder := post(t, handler.ApplyChanges, body); recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d", recorder.Code)
	}

	checkWrites(t, fake, `POST /my/products/example.com/dns/records/ {"comment":"Managed by External-DNS","data":"192.0.2.1","name":"www.example.com","ttl":3600,"type":"A"}`)
}

func TestUpdateKeepsTTLOfRewrittenRecordWhenUnset(t *testing.T) {
	handler, fake := newFakeHandler(t, exampleDomain, map[string][]map[string]any{
		"example.com": {record(1, "www", "A", "192.0.2.1", 300)},
	})

	body := `{"updateNew": [{"dnsName": "www.example.com", "recordType": "A", "targets": ["192.0.2.9"]}]}`
	if recorder := post(t, handler.ApplyChanges, body); recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d", recorder.Code)
	}

	checkWrites(t, fake, `PUT /my/products/example.com/dns/records/1/ {"comment":"Managed by External-DNS","data":"192.0.2.9","name":"www.example.com","ttl":300,"type":"A"}`)
}

func TestApplyChangesRunsOneAtATime(t *testing.T) {
	var running, peak atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := running.Add(1)
		defer running.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		time.Sleep(5 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"records": []}`))
	}))
	t.Cleanup(server.Close)

	client := simply.NewClient("secret")
	client.BaseURL = server.URL + "/"
	handler := NewHandler(client, slog.New(slog.NewTextHandler(io.Discard, nil)), exampleDomain)

	body := `{"create": [{"dnsName": "www.example.com", "recordType": "A", "targets": ["192.0.2.1"]}]}`
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			post(t, handler.ApplyChanges, body)
		}()
	}
	wg.Wait()

	if peak.Load() != 1 {
		t.Errorf("peak concurrent Simply.com requests = %d, want 1", peak.Load())
	}
}

func TestSetDomainsSwitchesManagedDomains(t *testing.T) {
	handler, _ := newFakeHandler(t, exampleDomain, map[string][]map[string]any{
		"example.com": {record(1, "www", "A", "192.0.2.1", 300)},
		"other.com":   {},
	})
	handler.SetDomains([]simply.Domain{{Object: "other.com", Name: "other.com"}})

	recorder := httptest.NewRecorder()
	handler.Negotiate(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := strings.TrimSpace(recorder.Body.String()); got != `{"domainFilter":["other.com"]}` {
		t.Errorf("negotiate = %s", got)
	}

	recorder = httptest.NewRecorder()
	handler.GetRecords(recorder, httptest.NewRequest(http.MethodGet, "/records", nil))
	if got := strings.TrimSpace(recorder.Body.String()); got != `[]` {
		t.Errorf("records = %s, want none from the dropped domain", got)
	}
}

func TestApplyChangesCanBeRetriedAfterAFailureHalfway(t *testing.T) {
	var mu sync.Mutex
	var stored []map[string]any
	var posts []string
	failNext := true

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"records": stored})
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		data := body["data"].(string)
		if data == "192.0.2.2" && failNext {
			failNext = false
			http.Error(w, "boom", http.StatusBadRequest)
			return
		}
		posts = append(posts, data)
		stored = append(stored, record(len(stored)+1, "www", "A", data, 300))
	}))
	defer server.Close()

	client := simply.NewClient("secret")
	client.BaseURL = server.URL + "/"
	handler := NewHandler(client, slog.New(slog.NewTextHandler(io.Discard, nil)), exampleDomain)

	body := `{"create": [{"dnsName": "www.example.com", "recordType": "A", "targets": ["192.0.2.1", "192.0.2.2"]}]}`
	if recorder := post(t, handler.ApplyChanges, body); recorder.Code != http.StatusInternalServerError {
		t.Fatalf("first call status = %d, want 500", recorder.Code)
	}
	if recorder := post(t, handler.ApplyChanges, body); recorder.Code != http.StatusNoContent {
		t.Fatalf("retry status = %d, want 204", recorder.Code)
	}

	if got := strings.Join(posts, ","); got != "192.0.2.1,192.0.2.2" {
		t.Errorf("created = %s, want each target exactly once", got)
	}
}

func TestCreateInUnmanagedDomainWritesNothing(t *testing.T) {
	handler, fake := newFakeHandler(t, exampleDomain, map[string][]map[string]any{"example.com": {}})

	body := `{"create": [{"dnsName": "www.other.org", "recordType": "A", "targets": ["192.0.2.1"]}]}`
	if recorder := post(t, handler.ApplyChanges, body); recorder.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", recorder.Code)
	}
	checkWrites(t, fake)
}
