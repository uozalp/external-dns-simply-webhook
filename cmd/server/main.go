package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/uozalp/external-dns-simply-webhook/pkg/simply"
	"github.com/uozalp/external-dns-simply-webhook/pkg/webhook"
)

const (
	domainRefreshInterval = 5 * time.Minute
	shutdownTimeout       = 30 * time.Second
)

// resolveDomains lists the Simply.com domains and narrows them to the comma-separated
// filter. An empty filter keeps every domain; a filter that matches none is an error.
func resolveDomains(client *simply.Client, logger *slog.Logger, filter string) ([]simply.Domain, error) {
	all, err := client.ListDomains()
	if err != nil {
		return nil, err
	}
	if filter == "" {
		return all, nil
	}

	found, unknown := simply.FilterDomains(all, strings.Split(filter, ","))
	for _, domain := range unknown {
		logger.Error("Domain filter: domain is NOT managed by Simply.com - skipping", "domain", domain)
	}
	if len(found) == 0 {
		return nil, errors.New("no valid domains found in DOMAIN_FILTER")
	}
	return found, nil
}

// refreshDomains keeps the handler's domains in step with the account, so a domain that
// is added or removed at Simply.com does not need a restart. A failed refresh keeps the old list.
func refreshDomains(ctx context.Context, client *simply.Client, logger *slog.Logger, handler *webhook.Handler, filter string) {
	ticker := time.NewTicker(domainRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			domains, err := resolveDomains(client, logger, filter)
			if err != nil {
				logger.Warn("Failed to refresh domains, keeping the current list", "error", err)
				continue
			}
			handler.SetDomains(domains)
		}
	}
}

func newRouter(handler *webhook.Handler) *http.ServeMux {
	router := http.NewServeMux()
	router.HandleFunc("GET /{$}", handler.Negotiate)
	router.HandleFunc("GET /healthz", handler.Healthz)
	router.HandleFunc("GET /records", handler.GetRecords)
	router.HandleFunc("POST /records", handler.ApplyChanges)
	router.HandleFunc("POST /adjustendpoints", handler.AdjustEndpoints)
	return router
}

func main() {
	// Configure logger
	// An empty or unknown LOG_LEVEL leaves the level at info.
	var level slog.Level
	_ = level.UnmarshalText([]byte(os.Getenv("LOG_LEVEL")))
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	}))

	// Get configuration from environment
	apiKey := os.Getenv("SIMPLY_API_KEY")
	if apiKey == "" {
		logger.Error("SIMPLY_API_KEY environment variable is required")
		os.Exit(1)
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8888"
	}

	// Create Simply.com client
	client := simply.NewClient(apiKey)

	domainFilter := os.Getenv("DOMAIN_FILTER")
	logger.Info("Fetching domains from Simply.com.")
	finalDomains, err := resolveDomains(client, logger, domainFilter)
	if err != nil {
		logger.Error("Failed to resolve domains", "error", err)
		os.Exit(1)
	}
	logger.Info("Managing domains", "count", len(finalDomains), "domains", finalDomains)

	// Create webhook handler with validated domains
	handler := webhook.NewHandler(client, logger, finalDomains)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go refreshDomains(ctx, client, logger, handler, domainFilter)

	// Setup router
	router := newRouter(handler)

	// Start server
	addr := ":" + port
	logger.Info("Starting server", "addr", addr)
	server := &http.Server{
		Addr:              addr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("Failed to shut down server", "error", err)
		}
	}()
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("Failed to start server", "error", err)
		os.Exit(1)
	}
	// ListenAndServe returns as soon as Shutdown starts; wait until in-flight requests finish.
	<-shutdownDone
}
