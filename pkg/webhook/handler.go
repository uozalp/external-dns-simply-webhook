package webhook

import (
	"encoding/json"
	"fmt"
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
	Client         *simply.Client
	DomainFilter   []string
	Logger         *logrus.Logger
	managedDomains []string
}

// NewHandler creates a new webhook handler
func NewHandler(client *simply.Client, domainFilter []string, logger *logrus.Logger) *Handler {
	return &Handler{
		Client:       client,
		DomainFilter: domainFilter,
		Logger:       logger,
	}
}

// GetRecords returns all current DNS records
func (h *Handler) GetRecords(w http.ResponseWriter, r *http.Request) {
	h.Logger.Info("GET /records called")

	// Fetch all records from Simply.com
	records, err := h.Client.ListAllRecords()
	if err != nil {
		h.Logger.Errorf("Failed to list records: %v", err)
		http.Error(w, fmt.Sprintf("Failed to list records: %v", err), http.StatusInternalServerError)
		return
	}

	// Convert to ExternalDNS endpoints
	endpoints := h.simplyToEndpoints(records)

	// Apply domain filter
	if len(h.DomainFilter) > 0 {
		endpoints = h.filterEndpoints(endpoints)
	}

	h.Logger.Infof("Returning %d records", len(endpoints))

	w.Header().Set("Content-Type", MediaTypeVersion)
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(endpoints)
}

// ApplyChanges applies the desired DNS record changes
func (h *Handler) ApplyChanges(w http.ResponseWriter, r *http.Request) {
	h.Logger.Info("POST /records called")

	var changes Changes
	if err := json.NewDecoder(r.Body).Decode(&changes); err != nil {
		h.Logger.Errorf("Failed to decode request body: %v", err)
		http.Error(w, fmt.Sprintf("Failed to decode request: %v", err), http.StatusBadRequest)
		return
	}

	h.Logger.Infof("Received %d desired records, %d current records",
		len(changes.DesiredRecords), len(changes.CurrentRecords))

	// Get available domains for FQDN parsing
	domains, err := h.getDomains()
	if err != nil {
		h.Logger.Errorf("Failed to get domains: %v", err)
		http.Error(w, fmt.Sprintf("Failed to get domains: %v", err), http.StatusInternalServerError)
		return
	}

	// Calculate the changes needed
	creates, updates, deletes := h.calculateChanges(changes.CurrentRecords, changes.DesiredRecords)

	h.Logger.Infof("Changes: %d creates, %d updates, %d deletes",
		len(creates), len(updates), len(deletes))

	// Apply deletions first
	for _, ep := range deletes {
		if err := h.deleteEndpoint(ep, domains); err != nil {
			h.Logger.Errorf("Failed to delete endpoint %s: %v", ep.DNSName, err)
			http.Error(w, fmt.Sprintf("Failed to delete record: %v", err), http.StatusInternalServerError)
			return
		}
	}

	// Apply updates
	for _, ep := range updates {
		if err := h.updateEndpoint(ep, domains); err != nil {
			h.Logger.Errorf("Failed to update endpoint %s: %v", ep.DNSName, err)
			http.Error(w, fmt.Sprintf("Failed to update record: %v", err), http.StatusInternalServerError)
			return
		}
	}

	// Apply creates
	for _, ep := range creates {
		if err := h.createEndpoint(ep, domains); err != nil {
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
	json.NewEncoder(w).Encode(endpoints)
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

// simplyToEndpoints converts Simply.com records to ExternalDNS endpoints
func (h *Handler) simplyToEndpoints(records []simply.Record) []*endpoint.Endpoint {
	var endpoints []*endpoint.Endpoint

	for _, record := range records {
		// Construct FQDN
		var fqdn string
		if record.Host == "@" || record.Host == "" {
			fqdn = record.Domain
		} else {
			fqdn = fmt.Sprintf("%s.%s", record.Host, record.Domain)
		}

		ep := &endpoint.Endpoint{
			DNSName:    fqdn,
			RecordType: record.Type,
			Targets:    []string{record.Data},
			RecordTTL:  endpoint.TTL(record.TTL),
		}

		// Store Simply.com record ID for updates/deletes
		if ep.ProviderSpecific == nil {
			ep.ProviderSpecific = make(endpoint.ProviderSpecific, 0)
		}
		ep.ProviderSpecific = append(ep.ProviderSpecific, endpoint.ProviderSpecificProperty{
			Name:  "simply-record-id",
			Value: fmt.Sprintf("%d", record.ID),
		})

		endpoints = append(endpoints, ep)
	}

	return endpoints
}

// filterEndpoints filters endpoints based on domain filter
func (h *Handler) filterEndpoints(endpoints []*endpoint.Endpoint) []*endpoint.Endpoint {
	var filtered []*endpoint.Endpoint
	for _, ep := range endpoints {
		for _, domain := range h.DomainFilter {
			if strings.HasSuffix(ep.DNSName, domain) {
				filtered = append(filtered, ep)
				break
			}
		}
	}
	return filtered
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

// getDomains returns the list of managed domains
func (h *Handler) getDomains() ([]string, error) {
	if len(h.managedDomains) > 0 {
		return h.managedDomains, nil
	}

	domains, err := h.Client.ListDomains()
	if err != nil {
		return nil, err
	}

	h.managedDomains = make([]string, len(domains))
	for i, d := range domains {
		h.managedDomains[i] = d.Name
	}

	return h.managedDomains, nil
}

// createEndpoint creates a new DNS record
func (h *Handler) createEndpoint(ep *endpoint.Endpoint, domains []string) error {
	host, domain := simply.ParseFQDN(ep.DNSName, domains)
	if domain == "" {
		return fmt.Errorf("could not determine domain for %s", ep.DNSName)
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
func (h *Handler) updateEndpoint(ep *endpoint.Endpoint, domains []string) error {
	host, domain := simply.ParseFQDN(ep.DNSName, domains)
	if domain == "" {
		return fmt.Errorf("could not determine domain for %s", ep.DNSName)
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
	// In a more sophisticated implementation, you might handle multiple targets differently
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
func (h *Handler) deleteEndpoint(ep *endpoint.Endpoint, domains []string) error {
	_, domain := simply.ParseFQDN(ep.DNSName, domains)
	if domain == "" {
		return fmt.Errorf("could not determine domain for %s", ep.DNSName)
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

	h.Logger.Infof("Deleting record: %s %s", ep.RecordType, ep.DNSName)
	if err := h.Client.DeleteRecord(recordID, domain); err != nil {
		return fmt.Errorf("failed to delete record: %w", err)
	}

	return nil
}
