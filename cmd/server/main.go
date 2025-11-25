package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/gorilla/mux"
	"github.com/uozalp/external-dns-simply-webhook/pkg/simply"
	"github.com/uozalp/external-dns-simply-webhook/pkg/webhook"
)

// corsMiddleware adds CORS headers to all responses
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Accept")
		w.Header().Set("Access-Control-Max-Age", "3600")

		// Handle preflight requests
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func main() {
	// Configure logger
	logLevel := os.Getenv("LOG_LEVEL")
	var level slog.Level
	switch strings.ToLower(logLevel) {
	case "debug":
		level = slog.LevelDebug
	case "info", "":
		level = slog.LevelInfo
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	}))

	// Get configuration from environment
	accountName := os.Getenv("SIMPLY_ACCOUNT_NAME")
	if accountName == "" {
		logger.Error("SIMPLY_ACCOUNT_NAME environment variable is required")
		os.Exit(1)
	}

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
	client := simply.NewClient(accountName, apiKey)

	// Fetch all domains managed by Simply.com
	logger.Info("Fetching domains from Simply.com.")
	allSimplyDomains, err := client.ListDomains()
	if err != nil {
		logger.Error("Failed to fetch domains from Simply.com", "error", err)
		os.Exit(1)
	}
	logger.Info("Found domains in Simply.com", "count", len(allSimplyDomains), "domains", allSimplyDomains)

	// Get domain filter from environment (optional)
	domainFilter := os.Getenv("DOMAIN_FILTER")
	var finalDomains []string

	if domainFilter != "" {
		// User specified domain filter - validate against Simply.com domains
		requestedDomains := strings.Split(domainFilter, ",")
		simplyDomainsMap := make(map[string]bool)
		for _, domain := range allSimplyDomains {
			simplyDomainsMap[domain] = true
		}

		for _, domain := range requestedDomains {
			domain = strings.TrimSpace(domain)
			if simplyDomainsMap[domain] {
				finalDomains = append(finalDomains, domain)
				logger.Info("Domain filter: domain is managed by Simply.com", "domain", domain)
			} else {
				logger.Error("Domain filter: domain is NOT managed by Simply.com - skipping", "domain", domain)
			}
		}

		if len(finalDomains) == 0 {
			logger.Error("No valid domains found in DOMAIN_FILTER")
			os.Exit(1)
		}
		logger.Info("Managing filtered domains", "count", len(finalDomains), "domains", finalDomains)
	} else {
		// No domain filter - manage all Simply.com domains
		finalDomains = allSimplyDomains
		logger.Info("No domain filter set, managing all Simply.com domains", "count", len(finalDomains))
	}

	// Create webhook handler with validated domains
	handler := webhook.NewHandler(client, logger, finalDomains)

	// Setup router
	router := mux.NewRouter()
	router.HandleFunc("/", handler.Negotiate).Methods("GET")
	router.HandleFunc("/healthz", handler.Healthz).Methods("GET")
	router.HandleFunc("/records", handler.GetRecords).Methods("GET")
	router.HandleFunc("/records", handler.ApplyChanges).Methods("POST")
	router.HandleFunc("/records", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}).Methods("OPTIONS")
	router.HandleFunc("/adjustendpoints", handler.AdjustEndpoints).Methods("POST")

	// Apply CORS middleware to all routes
	router.Use(corsMiddleware)

	// Start server
	addr := fmt.Sprintf(":%s", port)
	logger.Info("Starting server", "addr", addr)
	if err := http.ListenAndServe(addr, router); err != nil {
		logger.Error("Failed to start server", "error", err)
		os.Exit(1)
	}
}
