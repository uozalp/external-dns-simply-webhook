package webhook

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/sirupsen/logrus"
	"github.com/uozalp/external-dns-simply-webhook/pkg/simply"
	"sigs.k8s.io/external-dns/endpoint"
)

const (
	MediaTypeVersion = "application/external.dns.webhook+json;version=1"
)

// Handler handles webhook requests from ExternalDNS
type Handler struct {
	Client       *simply.Client
	Logger       *logrus.Logger
	DomainFilter []string
}

// NewHandler creates a new webhook handler
func NewHandler(client *simply.Client, logger *logrus.Logger, domainFilter []string) *Handler {
	return &Handler{
		Client:       client,
		Logger:       logger,
		DomainFilter: domainFilter,
	}
}

// GetRecords returns all current DNS records
func (h *Handler) GetRecords(w http.ResponseWriter, r *http.Request) {
	h.Logger.Info("GET /records called")

	type endpointResponse struct {
		DNSName    string   `json:"dnsName"`
		RecordType string   `json:"recordType"`
		Targets    []string `json:"targets"`
		TTL        int      `json:"ttl"`
	}

	var response []endpointResponse

	// Get records for each configured domain
	for _, domain := range h.DomainFilter {
		h.Logger.Infof("Fetching records for domain: %s", domain)

		records, err := h.Client.ListRecords(domain)
		if err != nil {
			h.Logger.Errorf("Failed to list records for domain %s: %v", domain, err)
			continue
		}

		h.Logger.Infof("Found %d records for domain %s", len(records), domain)

		// Convert Simply records to External-DNS endpoints
		for _, record := range records {
			h.Logger.Debugf("Processing record: ID=%d, Type=%s, Host=%s, Data=%s",
				record.ID, record.Type, record.Host, record.Data)

			// Build full DNS name
			var dnsName string
			if record.Host == "@" || record.Host == "" {
				dnsName = domain
			} else {
				dnsName = record.Host + "." + domain
			}

			ep := endpointResponse{
				DNSName:    dnsName,
				RecordType: record.Type,
				Targets:    []string{record.Data},
				TTL:        record.TTL,
			}
			response = append(response, ep)
		}
	}

	h.Logger.Infof("Returning %d records across %d domains", len(response), len(h.DomainFilter))

	w.Header().Set("Content-Type", MediaTypeVersion)
	w.WriteHeader(http.StatusOK)

	// Marshal to JSON first to avoid chunked encoding
	jsonData, err := json.Marshal(response)
	if err != nil {
		h.Logger.Errorf("Failed to marshal response: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	w.Write(jsonData)
}

// ApplyChanges applies the desired DNS record changes
func (h *Handler) ApplyChanges(w http.ResponseWriter, r *http.Request) {
	h.Logger.Info("POST /records called")

	// Read the raw body first for logging
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		h.Logger.Errorf("Failed to read request body: %v", err)
		http.Error(w, fmt.Sprintf("Failed to read request: %v", err), http.StatusBadRequest)
		return
	}

	// Log the raw payload
	h.Logger.Infof("Raw payload from ExternalDNS: %s", string(bodyBytes))

	var changes Changes
	if err := json.Unmarshal(bodyBytes, &changes); err != nil {
		h.Logger.Errorf("Failed to decode request body: %v", err)
		http.Error(w, fmt.Sprintf("Failed to decode request: %v", err), http.StatusBadRequest)
		return
	}

	h.Logger.Infof("Received %d desired records, %d current records",
		len(changes.DesiredRecords), len(changes.CurrentRecords))

	// Log each desired record in detail
	for i, record := range changes.DesiredRecords {
		h.Logger.Infof("Desired record %d: DNSName=%s, Type=%s, Targets=%v, TTL=%d",
			i+1, record.DNSName, record.RecordType, record.Targets, record.RecordTTL)
	}

	// Log each current record in detail
	for i, record := range changes.CurrentRecords {
		h.Logger.Infof("Current record %d: DNSName=%s, Type=%s, Targets=%v, TTL=%d",
			i+1, record.DNSName, record.RecordType, record.Targets, record.RecordTTL)
	}

	// Calculate the changes needed
	creates, updates, deletes := h.calculateChanges(changes.CurrentRecords, changes.DesiredRecords)

	h.Logger.Infof("Changes: %d creates, %d updates, %d deletes",
		len(creates), len(updates), len(deletes))

	// Apply deletions first
	for _, ep := range deletes {
		if err := h.deleteEndpoint(ep); err != nil {
			h.Logger.Errorf("Failed to delete endpoint %s: %v", ep.DNSName, err)
			http.Error(w, fmt.Sprintf("Failed to delete record: %v", err), http.StatusInternalServerError)
			return
		}
	}

	// Apply updates
	for _, ep := range updates {
		if err := h.updateEndpoint(ep); err != nil {
			h.Logger.Errorf("Failed to update endpoint %s: %v", ep.DNSName, err)
			http.Error(w, fmt.Sprintf("Failed to update record: %v", err), http.StatusInternalServerError)
			return
		}
	}

	// Apply creates
	for _, ep := range creates {
		if err := h.createEndpoint(ep); err != nil {
			h.Logger.Errorf("Failed to create endpoint %s: %v", ep.DNSName, err)
			http.Error(w, fmt.Sprintf("Failed to create record: %v", err), http.StatusInternalServerError)
			return
		}
	}

	h.Logger.Info("Successfully applied all changes")
	w.Header().Set("Content-Type", MediaTypeVersion)
	w.WriteHeader(http.StatusOK)
}

// AdjustEndpoints allows normalization or filtering of endpoints
func (h *Handler) AdjustEndpoints(w http.ResponseWriter, r *http.Request) {
	h.Logger.Info("POST /adjustendpoints called")

	var endpoints []*endpoint.Endpoint
	if err := json.NewDecoder(r.Body).Decode(&endpoints); err != nil {
		h.Logger.Errorf("Failed to decode request body: %v", err)
		http.Error(w, fmt.Sprintf("Failed to decode request: %v", err), http.StatusBadRequest)
		return
	}

	// Normalize endpoints (e.g., ensure trailing dots, lowercase)
	for _, ep := range endpoints {
		ep.DNSName = strings.ToLower(strings.TrimSuffix(ep.DNSName, "."))
	}

	w.Header().Set("Content-Type", MediaTypeVersion)
	w.WriteHeader(http.StatusOK)

	// Marshal to JSON first to avoid chunked encoding
	jsonData, err := json.Marshal(endpoints)
	if err != nil {
		h.Logger.Errorf("Failed to marshal response: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	w.Write(jsonData)
}

// Healthz returns health status
func (h *Handler) Healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

// Changes represents the change request from ExternalDNS
type Changes struct {
	DesiredRecords []*endpoint.Endpoint `json:"desiredRecords"`
	CurrentRecords []*endpoint.Endpoint `json:"currentRecords"`
}

// calculateChanges determines which endpoints need to be created, updated, or deleted
func (h *Handler) calculateChanges(current, desired []*endpoint.Endpoint) (creates, updates, deletes []*endpoint.Endpoint) {
	currentMap := make(map[string]*endpoint.Endpoint)
	for _, ep := range current {
		key := fmt.Sprintf("%s|%s", ep.DNSName, ep.RecordType)
		currentMap[key] = ep
	}

	desiredMap := make(map[string]*endpoint.Endpoint)
	for _, ep := range desired {
		key := fmt.Sprintf("%s|%s", ep.DNSName, ep.RecordType)
		desiredMap[key] = ep
	}

	// Find creates and updates
	for key, desiredEp := range desiredMap {
		if currentEp, exists := currentMap[key]; exists {
			// Check if update is needed
			if !endpointsEqual(currentEp, desiredEp) {
				// Copy the record ID from current to desired
				desiredEp.ProviderSpecific = currentEp.ProviderSpecific
				updates = append(updates, desiredEp)
			}
		} else {
			creates = append(creates, desiredEp)
		}
	}

	// Find deletes
	for key, currentEp := range currentMap {
		if _, exists := desiredMap[key]; !exists {
			deletes = append(deletes, currentEp)
		}
	}

	return creates, updates, deletes
}

// endpointsEqual checks if two endpoints are equal
func endpointsEqual(a, b *endpoint.Endpoint) bool {
	if a.DNSName != b.DNSName || a.RecordType != b.RecordType {
		return false
	}

	if len(a.Targets) != len(b.Targets) {
		return false
	}

	targetMap := make(map[string]bool)
	for _, t := range a.Targets {
		targetMap[t] = true
	}
	for _, t := range b.Targets {
		if !targetMap[t] {
			return false
		}
	}

	return a.RecordTTL == b.RecordTTL
}

// createEndpoint creates a new DNS record
func (h *Handler) createEndpoint(ep *endpoint.Endpoint) error {
	// Extract domain from DNS name (simple approach - last two parts)
	parts := strings.Split(ep.DNSName, ".")
	if len(parts) < 2 {
		return fmt.Errorf("invalid DNS name: %s", ep.DNSName)
	}

	domain := strings.Join(parts[len(parts)-2:], ".")
	host := strings.Join(parts[:len(parts)-2], ".")
	if host == "" {
		host = "@"
	}

	for _, target := range ep.Targets {
		record := simply.Record{
			Type:   ep.RecordType,
			Host:   host,
			Data:   target,
			TTL:    int(ep.RecordTTL),
			Domain: domain,
		}

		h.Logger.Infof("Creating record: %s %s %s (TTL: %d)", record.Type, ep.DNSName, target, record.TTL)
		if err := h.Client.AddRecord(record); err != nil {
			return fmt.Errorf("failed to add record: %w", err)
		}
	}

	return nil
}

// updateEndpoint updates an existing DNS record
func (h *Handler) updateEndpoint(ep *endpoint.Endpoint) error {
	// Extract domain from DNS name
	parts := strings.Split(ep.DNSName, ".")
	if len(parts) < 2 {
		return fmt.Errorf("invalid DNS name: %s", ep.DNSName)
	}

	domain := strings.Join(parts[len(parts)-2:], ".")
	host := strings.Join(parts[:len(parts)-2], ".")
	if host == "" {
		host = "@"
	}

	// Get the record ID from provider specific properties
	recordID := 0
	for _, prop := range ep.ProviderSpecific {
		if prop.Name == "simply-record-id" {
			fmt.Sscanf(prop.Value, "%d", &recordID)
			break
		}
	}

	if recordID == 0 {
		return fmt.Errorf("record ID not found for %s", ep.DNSName)
	}

	// For simplicity, we'll use the first target
	if len(ep.Targets) == 0 {
		return fmt.Errorf("no targets specified for %s", ep.DNSName)
	}

	record := simply.Record{
		ID:     recordID,
		Type:   ep.RecordType,
		Host:   host,
		Data:   ep.Targets[0],
		TTL:    int(ep.RecordTTL),
		Domain: domain,
	}

	h.Logger.Infof("Updating record: %s %s %s (TTL: %d)", record.Type, ep.DNSName, record.Data, record.TTL)
	if err := h.Client.UpdateRecord(record); err != nil {
		return fmt.Errorf("failed to update record: %w", err)
	}

	return nil
}

// deleteEndpoint deletes a DNS record
func (h *Handler) deleteEndpoint(ep *endpoint.Endpoint) error {
	// Extract domain from DNS name
	parts := strings.Split(ep.DNSName, ".")
	if len(parts) < 2 {
		return fmt.Errorf("invalid DNS name: %s", ep.DNSName)
	}

	domain := strings.Join(parts[len(parts)-2:], ".")

	// Get the record ID from provider specific properties
	recordID := 0
	for _, prop := range ep.ProviderSpecific {
		if prop.Name == "simply-record-id" {
			fmt.Sscanf(prop.Value, "%d", &recordID)
			break
		}
	}

	if recordID == 0 {
		return fmt.Errorf("record ID not found for %s", ep.DNSName)
	}

	h.Logger.Infof("Deleting record: %s %s", ep.RecordType, ep.DNSName)
	if err := h.Client.DeleteRecord(recordID, domain); err != nil {
		return fmt.Errorf("failed to delete record: %w", err)
	}

	return nil
}
