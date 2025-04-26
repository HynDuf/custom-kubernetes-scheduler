package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	v1 "k8s.io/api/core/v1" // Use official v1 types
)

// Struct for decoded JSON from HTTP response
type MetricResponse struct {
	Status string `json:"status"` // Add Status field for checking Prometheus response
	Data   Data   `json:"data"`
}

type Data struct {
	ResultType string   `json:"resultType"`
	Results    []Result `json:"result"`
}

type Result struct {
	MetricInfo map[string]string `json:"metric"` // Example: {"instance": "gke-node-1-...", "job": "kubernetes-pods", ...}
	MetricValue []interface{}     `json:"value"`  // Index 0 is unix_time (float64), index 1 is sample_value (string)
}

// --- Prometheus Configuration ---
const prometheusService = "http://prometheus-service.monitoring.svc.cluster.local:8080"
const prometheusQuery = "/api/v1/query?query=node_memory_MemAvailable_bytes"

// Returns the name of the node with the best metric value (maximum available memory).
// Takes a list of nodes that already passed predicate checks.
func getBestNodeName(ctx context.Context, compatibleNodes []v1.Node) (string, error) {
	if len(compatibleNodes) == 0 {
		return "", errors.New("no compatible nodes provided to select from")
	}

	// --- Query Prometheus ---
	queryURL := prometheusService + prometheusQuery
	log.Printf("Querying Prometheus: %s", queryURL)

	req, err := http.NewRequestWithContext(ctx, "GET", queryURL, nil)
	if err != nil {
		log.Printf("Error creating Prometheus request: %v", err)
		return "", fmt.Errorf("failed to create prometheus request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("Error querying Prometheus: %v", err)
		return "", fmt.Errorf("failed to query prometheus: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("Prometheus returned non-OK status: %s", resp.Status)
		return "", fmt.Errorf("prometheus query failed with status: %s", resp.Status)
	}

	// --- Decode Prometheus Response ---
	var metrics MetricResponse
	decoder := json.NewDecoder(resp.Body)
	err = decoder.Decode(&metrics)
	if err != nil {
		log.Printf("Error decoding Prometheus response: %v", err)
		return "", fmt.Errorf("failed to decode prometheus response: %w", err)
	}

	if metrics.Status != "success" {
		log.Printf("Prometheus query status was not 'success': %s", metrics.Status)
		return "", fmt.Errorf("prometheus query status indicates failure: %s", metrics.Status)
	}

	log.Printf("Prometheus returned %d metric results.", len(metrics.Data.Results))

	// --- Find Best Node based on Max Available Memory ---
	var maxAvailableMemory float64 = -1.0 // Use float64 for memory, initialize to -1
	bestNodeName := ""

	// Create a map of compatible node names for quick lookup
	compatibleNodeMap := make(map[string]struct{})
	for _, node := range compatibleNodes {
		compatibleNodeMap[node.Name] = struct{}{}
		log.Printf("Compatible node considered: %s", node.Name) // Log compatible nodes
	}

	// Iterate through Prometheus results
	for _, m := range metrics.Data.Results {
		// Prometheus node-exporter often uses 'instance' label which includes the port. Need to match against node name.
		// Sometimes it might just be 'node' label depending on relabeling. Check Prometheus UI for exact label name.
		// Assuming 'instance' label looks like 'gke-node-name-xyz:9100'
		instanceLabel, okInstance := m.MetricInfo["instance"]
		nodeLabel, okNode := m.MetricInfo["node"] // Check if 'node' label exists as fallback

		var nodeNameFromMetric string
		if okInstance {
			// Extract node name part from instance label (e.g., 'gke-node-name-xyz:9100' -> 'gke-node-name-xyz')
			// This assumes the port is always separated by ':'
            // Check if the instance label likely contains a port (look for the last colon)
            if portIndex := strings.LastIndex(instanceLabel, ":"); portIndex != -1 {
                // Simple check: if the part after colon looks like a number, assume it's a port
                if _, err := strconv.Atoi(instanceLabel[portIndex+1:]); err == nil {
                    nodeNameFromMetric = instanceLabel[:portIndex] // Extract name before the last colon
                    log.Printf("Parsed node name '%s' from instance label '%s'", nodeNameFromMetric, instanceLabel)
                } else {
                     // Found a colon, but suffix isn't a number - might be IPv6 or something else. Use as is? Or log warning?
                     log.Printf("Warning: Instance label '%s' contains ':' but suffix is not numeric. Using full label.", instanceLabel)
                     nodeNameFromMetric = instanceLabel // Fallback: Use the whole label if parsing fails
                }
            } else {
                // No colon found, assume the whole label is the node name (like in the example API output)
                nodeNameFromMetric = instanceLabel
                log.Printf("Using full instance label '%s' as node name (no port detected)", instanceLabel)
            }
		} else if okNode {
			nodeNameFromMetric = nodeLabel // Use 'node' label if 'instance' is missing
		} else {
			log.Printf("Skipping metric result, missing 'instance' or 'node' label: %v", m.MetricInfo)
			continue
		}

        if nodeNameFromMetric == "" { // Handle potential edge case where parsing resulted in empty string
             log.Printf("Warning: Could not determine node name from labels: %v", m.MetricInfo)
             continue
        }
		// Check if this node is one of the compatible nodes
		if _, isCompatible := compatibleNodeMap[nodeNameFromMetric]; !isCompatible {
			// log.Printf("Node %s from metric is not in the compatible list, skipping.", nodeNameFromMetric) // Optional: verbose logging
			continue
		}

		// Extract and parse the metric value (available memory)
		if len(m.MetricValue) < 2 {
			log.Printf("Skipping metric result for node %s, value array has unexpected length: %v", nodeNameFromMetric, m.MetricValue)
			continue
		}
		metricValueStr, ok := m.MetricValue[1].(string)
		if !ok {
			log.Printf("Skipping metric result for node %s, value is not a string: %T %v", nodeNameFromMetric, m.MetricValue[1], m.MetricValue[1])
			continue
		}

		metricValueFloat, err := strconv.ParseFloat(metricValueStr, 64)
		if err != nil {
			log.Printf("Error parsing metric value '%s' for node %s: %v", metricValueStr, nodeNameFromMetric, err)
			continue // Skip node if value is unparseable
		}

		log.Printf("Node: %s, Available Memory: %.0f bytes", nodeNameFromMetric, metricValueFloat)

		// Check if this node has more available memory than the current max
		if metricValueFloat > maxAvailableMemory {
			maxAvailableMemory = metricValueFloat
			bestNodeName = nodeNameFromMetric // Use the name derived from the metric label
		}
	}

	// --- Return Result ---
	if bestNodeName == "" {
		log.Println("Could not determine best node from Prometheus metrics among compatible nodes.")
		return "", errors.New("no suitable node found based on Prometheus metrics among the compatible options")
	}

	log.Printf("Selected best node: %s (Available Memory: %.0f bytes)", bestNodeName, maxAvailableMemory)
	return bestNodeName, nil
}
