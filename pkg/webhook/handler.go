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
	MediaTypeVersion     = "application/external.dns.webhook+json;version=1"
	ManagedByExternalDNS = "Managed by External-DNS"
	DefaultTTL           = 3600 // 1 hour
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

func (h *Handler) Negotiate(w http.ResponseWriter, r *http.Request) {
	// Respond with the supported media type version
	response := map[string]interface{}{
		"domainFilter": h.DomainFilter,
	}

	jsonData, err := json.Marshal(response)
	if err != nil {
		h.Logger.Errorf("Failed to marshal negotiation response: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", MediaTypeVersion)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(jsonData)))
	w.WriteHeader(http.StatusOK)
	w.Write(jsonData)
}

// GetRecords returns all current DNS records
func (h *Handler) GetRecords(w http.ResponseWriter, r *http.Request) {

	type endpointResponse struct {
		DNSName    string   `json:"dnsName"`
		RecordTTL  int      `json:"recordTTL"`
		RecordType string   `json:"recordType"`
		Targets    []string `json:"targets"`
	}

	var response []endpointResponse

	// Get records for each configured domain
	for _, domain := range h.DomainFilter {

		records, err := h.Client.ListRecords(domain)
		if err != nil {
			h.Logger.Errorf("Failed to list records for domain %s: %v", domain, err)
			continue
		}

		h.Logger.Debugf("Found %d records for domain %s", len(records), domain)

		// Convert Simply records to External-DNS endpoints
		for _, record := range records {
			h.Logger.Debugf("Processing record: ID=%d, Type=%s, Name=%s, Data=%s",
				record.ID, record.Type, record.Name, record.Data)

			// Build full DNS name
			var dnsName string
			if record.Name == "@" || record.Name == "" {
				dnsName = domain
			} else {
				dnsName = record.Name + "." + domain
			}

			ep := endpointResponse{
				DNSName:    dnsName,
				RecordType: record.Type,
				Targets:    []string{record.Data},
				RecordTTL:  record.TTL,
			}
			response = append(response, ep)
		}
	}

	h.Logger.Infof("Returning %d records across %d domains", len(response), len(h.DomainFilter))

	// Marshal to JSON first to avoid chunked encoding
	jsonData, err := json.Marshal(response)
	if err != nil {
		h.Logger.Errorf("Failed to marshal response: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", MediaTypeVersion)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(jsonData)))
	w.WriteHeader(http.StatusOK)
	w.Write(jsonData)
}

// ApplyChanges applies the desired DNS record changes
func (h *Handler) ApplyChanges(w http.ResponseWriter, r *http.Request) {

	// Define the request structure
	type Changes struct {
		Create    []*endpoint.Endpoint `json:"create"`
		UpdateOld []*endpoint.Endpoint `json:"updateOld"`
		UpdateNew []*endpoint.Endpoint `json:"updateNew"`
		Delete    []*endpoint.Endpoint `json:"delete"`
	}

	var changes Changes
	if err := json.NewDecoder(r.Body).Decode(&changes); err != nil {
		h.Logger.Errorf("Failed to decode request body: %v", err)
		http.Error(w, fmt.Sprintf("Failed to decode request: %v", err), http.StatusBadRequest)
		return
	}

	h.Logger.Infof("Received changes: %d creates, %d updates, %d deletes",
		len(changes.Create), len(changes.UpdateNew), len(changes.Delete))

	// Log the full request for debugging
	reqJSON, _ := json.MarshalIndent(changes, "", "  ")
	h.Logger.Debugf("Full request payload:\n%s", string(reqJSON))

	// Fetch all records from all domains and build a lookup map
	// Key: dnsName:recordType, Value: simply.Record
	recordMap := make(map[string]simply.Record)

	for _, domain := range h.DomainFilter {
		records, err := h.Client.ListRecords(domain)
		if err != nil {
			h.Logger.Errorf("Failed to list records for domain %s: %v", domain, err)
			http.Error(w, fmt.Sprintf("Failed to list records: %v", err), http.StatusInternalServerError)
			return
		}

		for _, record := range records {
			// Build full DNS name
			var dnsName string
			if record.Name == "@" || record.Name == "" {
				dnsName = domain
			} else {
				dnsName = record.Name + "." + domain
			}

			// Create lookup key: dnsName:recordType
			key := fmt.Sprintf("%s:%s", dnsName, record.Type)
			recordMap[key] = record
		}
	}

	// Process creates
	for _, ep := range changes.Create {
		if err := h.createEndpoint(ep); err != nil {
			h.Logger.Errorf("Failed to create endpoint %s: %v", ep.DNSName, err)
			http.Error(w, fmt.Sprintf("Failed to create record: %v", err), http.StatusInternalServerError)
			return
		}
	}

	// Process updates - compare old and new to detect actual changes
	for i, newEp := range changes.UpdateNew {
		oldEp := changes.UpdateOld[i]

		// Check if there are actual changes
		hasChanges := false
		if oldEp.RecordType != newEp.RecordType {
			hasChanges = true
		} else if oldEp.RecordTTL != newEp.RecordTTL {
			hasChanges = true
		} else {
			// Compare targets
			for j, oldTarget := range oldEp.Targets {
				if j >= len(newEp.Targets) || oldTarget != newEp.Targets[j] {
					hasChanges = true
					break
				}
			}
		}

		if !hasChanges {
			h.Logger.Infof("Skipping update for %s %s - no actual changes detected",
				newEp.DNSName, newEp.RecordType)
			continue
		}

		// Lookup record ID from map
		key := fmt.Sprintf("%s:%s", newEp.DNSName, newEp.RecordType)
		existingRecord, found := recordMap[key]
		if !found {
			h.Logger.Errorf("Record not found in map for update: %s", key)
			http.Error(w, fmt.Sprintf("Record not found: %s", key), http.StatusInternalServerError)
			return
		}

		if err := h.updateEndpoint(newEp, existingRecord.ID); err != nil {
			h.Logger.Errorf("Failed to update endpoint %s: %v", newEp.DNSName, err)
			http.Error(w, fmt.Sprintf("Failed to update record: %v", err), http.StatusInternalServerError)
			return
		}
	}

	// Process deletes - lookup record ID from map
	for _, ep := range changes.Delete {
		key := fmt.Sprintf("%s:%s", ep.DNSName, ep.RecordType)
		existingRecord, found := recordMap[key]
		if !found {
			h.Logger.Warnf("Record not found in map for deletion, skipping: %s", key)
			continue
		}

		if err := h.deleteEndpoint(ep, existingRecord.ID); err != nil {
			h.Logger.Errorf("Failed to delete endpoint %s: %v", ep.DNSName, err)
			http.Error(w, fmt.Sprintf("Failed to delete record: %v", err), http.StatusInternalServerError)
			return
		}
	}

	h.Logger.Info("Successfully applied all changes")
	w.WriteHeader(http.StatusNoContent)
}

// AdjustEndpoints allows normalization or filtering of endpoints
func (h *Handler) AdjustEndpoints(w http.ResponseWriter, r *http.Request) {

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

	// Marshal to JSON first to avoid chunked encoding
	jsonData, err := json.Marshal(endpoints)
	if err != nil {
		h.Logger.Errorf("Failed to marshal response: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", MediaTypeVersion)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(jsonData)))
	w.WriteHeader(http.StatusOK)
	w.Write(jsonData)
}

// Healthz returns health status
func (h *Handler) Healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

// createEndpoint creates a new DNS record
func (h *Handler) createEndpoint(ep *endpoint.Endpoint) error {
	domain, err := h.extractDomain(ep.DNSName)
	if err != nil {
		return err
	}

	// Set default TTL if not specified
	ttl := int(ep.RecordTTL)
	if ttl == 0 {
		ttl = DefaultTTL
	}

	// Create record for each target
	for _, target := range ep.Targets {
		record := simply.Record{
			Type:    ep.RecordType,
			Name:    ep.DNSName,
			Data:    target,
			TTL:     ttl,
			Comment: ManagedByExternalDNS,
		}

		h.Logger.Infof("Creating Simply.com record: domain=%s, name=%s, type=%s, data=%s, ttl=%d",
			domain, record.Name, record.Type, record.Data, record.TTL)

		if err := h.Client.AddRecord(domain, record); err != nil {
			return fmt.Errorf("failed to add record: %w", err)
		}
	}

	return nil
}

// updateEndpoint updates an existing DNS record
func (h *Handler) updateEndpoint(ep *endpoint.Endpoint, recordID int) error {
	domain, err := h.extractDomain(ep.DNSName)
	if err != nil {
		return err
	}

	if len(ep.Targets) == 0 {
		return fmt.Errorf("no targets specified for update")
	}

	// Set default TTL if not specified
	ttl := int(ep.RecordTTL)
	if ttl == 0 {
		ttl = DefaultTTL
	}

	record := simply.Record{
		ID:      recordID,
		Type:    ep.RecordType,
		Name:    ep.DNSName,
		Data:    ep.Targets[0],
		TTL:     ttl,
		Comment: ManagedByExternalDNS,
	}

	h.Logger.Infof("Updating Simply.com record: id=%d, domain=%s, name=%s, type=%s, data=%s, ttl=%d",
		recordID, domain, record.Name, record.Type, record.Data, record.TTL)

	if err := h.Client.UpdateRecord(domain, record); err != nil {
		return fmt.Errorf("failed to update record: %w", err)
	}

	return nil
}

// deleteEndpoint deletes a DNS record
func (h *Handler) deleteEndpoint(ep *endpoint.Endpoint, recordID int) error {
	domain, err := h.extractDomain(ep.DNSName)
	if err != nil {
		return err
	}

	record := simply.Record{
		ID:      recordID,
		Type:    ep.RecordType,
		Name:    ep.DNSName,
		Data:    ep.Targets[0],
		TTL:     int(ep.RecordTTL),
		Comment: ManagedByExternalDNS,
	}

	h.Logger.Infof("Deleting Simply.com record id=%d, domain=%s, name=%s, type=%s",
		recordID, domain, record.Name, record.Type)

	if err := h.Client.DeleteRecord(domain, record); err != nil {
		return fmt.Errorf("failed to delete record: %w", err)
	}

	return nil
}

// extractDomain extracts the base domain from a DNS name
func (h *Handler) extractDomain(dnsName string) (string, error) {
	parts := strings.Split(dnsName, ".")
	if len(parts) < 2 {
		return "", fmt.Errorf("invalid DNS name: %s", dnsName)
	}
	return strings.Join(parts[len(parts)-2:], "."), nil
}
