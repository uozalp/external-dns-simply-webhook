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

	// Create Simply.com client
	client := simply.NewClient(accountName, apiKey)

	domainFilter := os.Getenv("DOMAIN_FILTER")
	var domainFilterList []string
	if domainFilter != "" {
		domainFilterList = strings.Split(domainFilter, ",")
		for i := range domainFilterList {
			domainFilterList[i] = strings.TrimSpace(domainFilterList[i])
		}
		logger.Infof("Domain filter enabled: %v", domainFilterList)
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// Create webhook handler
	handler := webhook.NewHandler(client, logger, domainFilterList)

	// Setup router
	router := mux.NewRouter()
	router.HandleFunc("/healthz", handler.Healthz).Methods("GET")
	router.HandleFunc("/records", handler.GetRecords).Methods("GET")
	router.HandleFunc("/records", handler.ApplyChanges).Methods("POST")
	router.HandleFunc("/adjustendpoints", handler.AdjustEndpoints).Methods("POST")

	// Start server
	addr := fmt.Sprintf(":%s", port)
	logger.Infof("Starting server on %s", addr)
	if err := http.ListenAndServe(addr, router); err != nil {
		logger.Fatalf("Failed to start server: %v", err)
	}
}
