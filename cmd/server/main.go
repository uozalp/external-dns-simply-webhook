package main

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
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
	logger := logrus.New()
	logger.SetFormatter(&logrus.JSONFormatter{})
	logger.SetOutput(os.Stdout)

	logLevel := os.Getenv("LOG_LEVEL")
	if logLevel == "" {
		logLevel = "info"
	}
	level, err := logrus.ParseLevel(logLevel)
	if err != nil {
		logger.Warnf("Invalid log level %s, defaulting to info", logLevel)
		level = logrus.InfoLevel
	}
	logger.SetLevel(level)

	// Get configuration from environment
	accountName := os.Getenv("SIMPLY_ACCOUNT_NAME")
	if accountName == "" {
		logger.Fatal("SIMPLY_ACCOUNT_NAME environment variable is required")
	}

	apiKey := os.Getenv("SIMPLY_API_KEY")
	if apiKey == "" {
		logger.Fatal("SIMPLY_API_KEY environment variable is required")
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
		logger.Fatalf("Failed to fetch domains from Simply.com: %v", err)
	}
	logger.Infof("Found %d domains in Simply.com: %v", len(allSimplyDomains), allSimplyDomains)

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
				logger.Infof("Domain filter: %s is managed by Simply.com", domain)
			} else {
				logger.Errorf("Domain filter: %s is NOT managed by Simply.com - skipping", domain)
			}
		}

		if len(finalDomains) == 0 {
			logger.Fatal("No valid domains found in DOMAIN_FILTER")
		}
		logger.Infof("Managing %d filtered domains: %v", len(finalDomains), finalDomains)
	} else {
		// No domain filter - manage all Simply.com domains
		finalDomains = allSimplyDomains
		logger.Infof("No domain filter set, managing all %d Simply.com domains", len(finalDomains))
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
	logger.Infof("Starting server on %s", addr)
	if err := http.ListenAndServe(addr, router); err != nil {
		logger.Fatalf("Failed to start server: %v", err)
	}
}
