package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/uozalp/external-dns-simply-webhook/pkg/simply"
	"github.com/uozalp/external-dns-simply-webhook/pkg/webhook"
)

func TestRouter(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := newRouter(webhook.NewHandler(simply.NewClient("secret"), logger, nil))

	tests := []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/", http.StatusOK},
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodGet, "/unknown", http.StatusNotFound},
		{http.MethodDelete, "/records", http.StatusMethodNotAllowed},
		{http.MethodGet, "/adjustendpoints", http.StatusMethodNotAllowed},
		{http.MethodPost, "/adjustendpoints", http.StatusOK},
	}
	for _, tc := range tests {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, strings.NewReader("[]")))
		if recorder.Code != tc.want {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, recorder.Code, tc.want)
		}
	}
}

func newDomainServer(t *testing.T) *simply.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"products": [
			{"object": "example.com", "domain": {"name": "example.com", "name_idn": "example.com", "managed": true}},
			{"object": "other.com", "domain": {"name": "other.com", "name_idn": "other.com", "managed": true}}
		]}`))
	}))
	t.Cleanup(server.Close)

	client := simply.NewClient("secret")
	client.BaseURL = server.URL + "/"
	return client
}

func TestResolveDomains(t *testing.T) {
	client := newDomainServer(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	all, err := resolveDomains(client, logger, "")
	if err != nil || len(all) != 2 {
		t.Errorf("no filter: domains = %v, err = %v, want both", all, err)
	}

	some, err := resolveDomains(client, logger, "Example.com, missing.com")
	if err != nil || len(some) != 1 || some[0].Object != "example.com" {
		t.Errorf("filter: domains = %v, err = %v, want example.com only", some, err)
	}

	if _, err := resolveDomains(client, logger, "missing.com"); err == nil {
		t.Error("a filter that matches nothing must be an error")
	}
}
