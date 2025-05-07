package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io" // Import for io.ReadAll
	"log"
	"math"
	"net/http"
	"net/url" // Import for url.Parse and url.Values
	"sort"
	"strconv"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection" // For NodeSelectorRequirementsAsSelector
	// client-go kubernetes not directly needed here unless for a K8s client
)

// ScoringConfig holds the weights for different scoring metrics.
type ScoringConfig struct {
	WeightMem      float64
	WeightCPU      float64
	WeightAffinity float64
}

// NodeScore holds the individual and combined scores for a node.
type NodeScore struct {
	Name          string
	MemScore      float64
	CPUScore      float64
	AffinityScore float64
	TotalScore    float64
}

// Prometheus metric response structure
type MetricResponse struct {
	Status string `json:"status"`
	Data   Data   `json:"data"`
	// For error responses from Prometheus
	ErrorType string `json:"errorType,omitempty"`
	Error     string `json:"error,omitempty"`
}

type Data struct {
	ResultType string   `json:"resultType"`
	Results    []Result `json:"result"`
}

type Result struct {
	MetricInfo  map[string]string `json:"metric"`
	MetricValue []interface{}     `json:"value"`
}

const (
	prometheusService = "http://prometheus-service.monitoring.svc.cluster.local:8080"
	// PromQL queries (without the /api/v1/query?query= part)
	memPromQL           = "node_memory_MemAvailable_bytes"
	cpuIdlePromQL       = `avg by (instance) (rate(node_cpu_seconds_total{mode="idle"}[1m]))` // Avg Idle Cores
	maxScore      int   = 10
	minScore      int   = 0
)

// queryPrometheus sends a PromQL query to Prometheus and parses the response.
// promQL should be the raw PromQL string (e.g., "node_memory_MemAvailable_bytes").
func queryPrometheus(ctx context.Context, promQL string) (*MetricResponse, error) {
	baseAPIURL := prometheusService + "/api/v1/query"

	// Prepare URL with properly encoded query parameter
	parsedURL, err := url.Parse(baseAPIURL)
	if err != nil {
		// This should not happen with a hardcoded baseAPIURL
		log.Printf("FATAL: Failed to parse base Prometheus URL '%s': %v", baseAPIURL, err)
		return nil, fmt.Errorf("internal error parsing prometheus base url: %w", err)
	}
	queryParams := parsedURL.Query()
	queryParams.Set("query", promQL)
	parsedURL.RawQuery = queryParams.Encode() // This correctly URL-encodes the promQL string

	fullURL := parsedURL.String()
	log.Printf("Querying Prometheus: %s", fullURL) // Log the fully constructed and encoded URL

	req, err := http.NewRequestWithContext(ctx, "GET", fullURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create prometheus request for %s: %w", promQL, err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to query prometheus for %s: %w", promQL, err)
	}
	defer resp.Body.Close()

	bodyBytes, readErr := io.ReadAll(resp.Body) // Read the body ONCE
	if readErr != nil {
		log.Printf("Prometheus query for '%s' returned status %s. Failed to read response body: %v", promQL, resp.Status, readErr)
		return nil, fmt.Errorf("prometheus query for '%s' failed with status %s and unreadable body", promQL, resp.Status)
	}

	if resp.StatusCode != http.StatusOK {
		// Log the raw body for non-OK responses
		log.Printf("Prometheus query for '%s' returned non-OK status: %s. Raw Body: %s", promQL, resp.Status, string(bodyBytes))
		// Attempt to parse as MetricResponse anyway, as Prometheus might still send structured error
		var metrics MetricResponse
		if unmarshalErr := json.Unmarshal(bodyBytes, &metrics); unmarshalErr == nil && metrics.Error != "" {
			return nil, fmt.Errorf("prometheus query for '%s' failed with status %s: %s (%s)", promQL, resp.Status, metrics.Error, metrics.ErrorType)
		}
		return nil, fmt.Errorf("prometheus query for '%s' failed with status %s", promQL, resp.Status)
	}

	var metrics MetricResponse
	if err := json.Unmarshal(bodyBytes, &metrics); err != nil {
		log.Printf("Error decoding Prometheus response for '%s'. Raw Body: %s", promQL, string(bodyBytes))
		return nil, fmt.Errorf("failed to decode prometheus response for '%s': %w", promQL, err)
	}

	if metrics.Status != "success" {
		log.Printf("Prometheus query for '%s' status was not 'success': %s. ErrorType: %s, Error: %s. Full Response: %+v",
			promQL, metrics.Status, metrics.ErrorType, metrics.Error, metrics)
		return nil, fmt.Errorf("prometheus query for '%s' status indicates failure: %s (%s: %s)", promQL, metrics.Status, metrics.ErrorType, metrics.Error)
	}

	log.Printf("Prometheus query for '%s' returned %d results.", promQL, len(metrics.Data.Results))
	return &metrics, nil
}

// Parses the 'instance' or 'node' label from Prometheus result to get the k8s node name.
func getNodeNameFromMetric(metricInfo map[string]string) (string, error) {
	// Priority: 'instance', then 'node'
	// The Prometheus relabeling for 'kubernetes-pods' job in your config-map.yaml
	// sets `target_label: instance` from `__meta_kubernetes_pod_node_name`.
	// So, 'instance' should be the Kubernetes node name.
	instanceLabel, okInstance := metricInfo["instance"]
	nodeLabel, okNode := metricInfo["node"] // Fallback if 'instance' is missing

	var nodeNameSource string
	var rawLabelValue string

	if okInstance && instanceLabel != "" {
		nodeNameSource = "instance"
		rawLabelValue = instanceLabel
	} else if okNode && nodeLabel != "" {
		nodeNameSource = "node"
		rawLabelValue = nodeLabel
	} else {
		return "", fmt.Errorf("metric result missing a usable 'instance' or 'node' label: %v", metricInfo)
	}

	// The 'instance' label (from __meta_kubernetes_pod_node_name) should be the node name directly.
	// It might sometimes include a port if the relabeling is different or from another job.
	// parseInstanceLabel will strip the port if it looks like one.
	parsedNodeName := parseInstanceLabel(rawLabelValue)

	if parsedNodeName == "" {
		return "", fmt.Errorf("could not determine node name from label '%s' (value: '%s'): %v", nodeNameSource, rawLabelValue, metricInfo)
	}
	// log.Printf("Debug: getNodeNameFromMetric: label '%s', value '%s', parsed '%s'", nodeNameSource, rawLabelValue, parsedNodeName)
	return parsedNodeName, nil
}

// Helper to parse instance/node label, removing port if present
func parseInstanceLabel(label string) string {
	if portIndex := strings.LastIndex(label, ":"); portIndex != -1 {
		// Check if the part after colon is purely numeric (a port)
		potentialPort := label[portIndex+1:]
		if _, err := strconv.Atoi(potentialPort); err == nil {
			// It's a numeric port, strip it
			return label[:portIndex]
		}
		// If not purely numeric, it might be part of an IPv6 address or other format.
		// In such cases, it's safer to return the original label, assuming it's the intended identifier.
		// log.Printf("Debug: parseInstanceLabel: found colon in '%s', but suffix '%s' is not a simple port. Returning full label.", label, potentialPort)
		return label
	}
	// No colon found, assume the whole label is the node name
	return label
}

// Fetches Memory and CPU metrics for the given compatible nodes.
func fetchNodeMetrics(ctx context.Context, compatibleNodeNames map[string]struct{}) (map[string]float64, map[string]float64, error) {
	memValues := make(map[string]float64)
	cpuValues := make(map[string]float64) // Idle cores

	// Fetch Memory
	memMetrics, err := queryPrometheus(ctx, memPromQL)
	if err != nil {
		// Log details, but allow scheduler to proceed if one metric type fails, scoring with 0 for that metric.
		log.Printf("Warning: Failed fetching memory metrics, they will be scored as 0: %v", err)
	} else {
		for _, m := range memMetrics.Data.Results {
			nodeName, errName := getNodeNameFromMetric(m.MetricInfo)
			if errName != nil {
				log.Printf("Skipping memory metric due to name parsing error: %v. Metric: %v", errName, m.MetricInfo)
				continue
			}
			if _, isCompatible := compatibleNodeNames[nodeName]; !isCompatible {
				// log.Printf("Debug: Node '%s' from memory metric not in compatible list. Skipping.", nodeName)
				continue
			}
			val, errVal := parseMetricValue(m.MetricValue)
			if errVal != nil {
				log.Printf("Skipping memory metric for node %s due to value parsing error: %v", nodeName, errVal)
				continue
			}
			memValues[nodeName] = val
		}
	}

	// Fetch CPU Idle
	cpuMetrics, err := queryPrometheus(ctx, cpuIdlePromQL)
	if err != nil {
		log.Printf("Warning: Failed fetching CPU metrics, they will be scored as 0: %v", err)
	} else {
		for _, m := range cpuMetrics.Data.Results {
			nodeName, errName := getNodeNameFromMetric(m.MetricInfo)
			if errName != nil {
				log.Printf("Skipping CPU metric due to name parsing error: %v. Metric: %v", errName, m.MetricInfo)
				continue
			}
			if _, isCompatible := compatibleNodeNames[nodeName]; !isCompatible {
				// log.Printf("Debug: Node '%s' from CPU metric not in compatible list. Skipping.", nodeName)
				continue
			}
			val, errVal := parseMetricValue(m.MetricValue)
			if errVal != nil {
				log.Printf("Skipping CPU metric for node %s due to value parsing error: %v", nodeName, errVal)
				continue
			}
			cpuValues[nodeName] = val
		}
	}
	if len(memValues) == 0 && len(cpuValues) == 0 && (memMetrics == nil || cpuMetrics == nil) {
		// Both metric fetches failed completely
		return nil, nil, fmt.Errorf("all node metric fetches failed (memory and CPU)")
	}

	return memValues, cpuValues, nil
}

// Parses the value[1] (string) from Prometheus result into float64
func parseMetricValue(metricValue []interface{}) (float64, error) {
	if len(metricValue) < 2 {
		return 0, fmt.Errorf("value array has unexpected length: %v", metricValue)
	}
	metricValueStr, ok := metricValue[1].(string)
	if !ok {
		return 0, fmt.Errorf("value is not a string: %T %v", metricValue[1], metricValue[1])
	}
	metricValueFloat, err := strconv.ParseFloat(metricValueStr, 64)
	if err != nil {
		return 0, fmt.Errorf("error parsing metric value '%s': %w", metricValueStr, err)
	}
	return metricValueFloat, nil
}

// Normalizes a value to a 0-10 scale based on the max value found.
func normalizeScore(value, maxValue float64) float64 {
	if maxValue <= 1e-9 { // Avoid division by zero or very small numbers, treat as min score
		return float64(minScore)
	}
	if value < 0 { // If value is negative (e.g. bad metric data), give min score
		return float64(minScore)
	}
	score := (value / maxValue) * float64(maxScore)
	return math.Max(float64(minScore), math.Min(score, float64(maxScore))) // Clamp between minScore and maxScore
}

// Calculate the affinity score bonus for a node based on pod preferences.
func calculateAffinityScore(node *v1.Node, pod *v1.Pod) (float64, error) {
	affinity := pod.Spec.Affinity
	if affinity == nil || affinity.NodeAffinity == nil || len(affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution) == 0 {
		return 0, nil // No preference defined
	}

	var totalPreferenceScore int32 = 0
	nodeLabelsSet := labels.Set(node.Labels)

	for _, preferredTerm := range affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
		// Skip if the preference term itself is nil (should not happen with valid K8s spec)
		// if preferredTerm.Preference == nil { continue }

		// MatchExpressions logic
		if preferredTerm.Preference.MatchExpressions != nil {
			nodeSelector, err := NodeSelectorRequirementsAsSelector(preferredTerm.Preference.MatchExpressions)
			if err != nil {
				log.Printf("Warning: Failed to parse MatchExpressions for node %s, pod %s/%s: %v", node.Name, pod.Namespace, pod.Name, err)
				continue
			}
			if nodeSelector.Matches(nodeLabelsSet) {
				totalPreferenceScore += preferredTerm.Weight
			}
		}
		// MatchFields logic (less common for preferred node affinity)
		if preferredTerm.Preference.MatchFields != nil {
			// Kubernetes' default scheduler doesn't support MatchFields for NodeAffinity.
			// Implementing this would require evaluating fields on the node object.
			// For now, we can log a warning or ignore.
			log.Printf("Warning: MatchFields in preferredNodeAffinity is not fully supported by this custom scheduler. Node: %s, Pod: %s/%s", node.Name, pod.Namespace, pod.Name)
		}
	}

	// Normalize the affinity score (0-10). Max possible raw score could be high (e.g. 100 * num_terms).
	// Simple scaling: if max possible weight per term is 100, and we expect 1-2 terms usually.
	// Let's scale assuming a max sum of 100 gives full score (10).
	// This makes the weight parameter more intuitive (1.0 weight means affinity bonus can be up to 10).
	scaledScore := (float64(totalPreferenceScore) / 100.0) * float64(maxScore)
	scaledScore = math.Max(0, math.Min(scaledScore, float64(maxScore))) // Clamp 0-10

	if totalPreferenceScore > 0 {
		log.Printf("Node %s affinity score (raw sum: %d, scaled 0-10: %.2f)", node.Name, totalPreferenceScore, scaledScore)
	}
	return scaledScore, nil
}

// NodeSelectorRequirementsAsSelector converts the []NodeSelectorRequirement api type into a struct that implements
// labels.Selector.
func NodeSelectorRequirementsAsSelector(nsm []v1.NodeSelectorRequirement) (labels.Selector, error) {
	if len(nsm) == 0 {
		return labels.Nothing(), nil
	}
	selector := labels.NewSelector()
	for _, expr := range nsm {
		var op selection.Operator
		switch expr.Operator {
		case v1.NodeSelectorOpIn:
			op = selection.In
		case v1.NodeSelectorOpNotIn:
			op = selection.NotIn
		case v1.NodeSelectorOpExists:
			op = selection.Exists
		case v1.NodeSelectorOpDoesNotExist:
			op = selection.DoesNotExist
		case v1.NodeSelectorOpGt:
			op = selection.GreaterThan
		case v1.NodeSelectorOpLt:
			op = selection.LessThan
		default:
			return nil, fmt.Errorf("%q is not a valid node selector operator", expr.Operator)
		}
		r, err := labels.NewRequirement(expr.Key, op, expr.Values)
		if err != nil {
			return nil, err
		}
		selector = selector.Add(*r)
	}
	return selector, nil
}

// ScoreNodes calculates scores for all compatible nodes and returns the name of the best node.
func ScoreNodes(ctx context.Context, compatibleNodes []v1.Node, pod *v1.Pod, config ScoringConfig) (string, error) {
	if len(compatibleNodes) == 0 {
		return "", errors.New("no compatible nodes to score")
	}

	compatibleNodeMap := make(map[string]struct{})
	for _, node := range compatibleNodes {
		compatibleNodeMap[node.Name] = struct{}{}
	}

	memValues, cpuValues, err := fetchNodeMetrics(ctx, compatibleNodeMap)
	if err != nil {
		// err is already logged by fetchNodeMetrics if specific fetches fail.
		// If all fetches failed, fetchNodeMetrics returns an error.
		log.Printf("Continuing scoring with potentially incomplete metrics due to: %v", err)
		// If BOTH memValues and cpuValues are nil (total failure), then we should error out.
		if memValues == nil && cpuValues == nil {
			return "", fmt.Errorf("cannot score nodes, all metric fetching failed: %w", err)
		}
	}

	maxMem := 0.0
	maxCPU := 0.0
	// Only iterate over nodes for which we successfully got metrics for finding max.
	for nodeName := range compatibleNodeMap { // Iterate over names to ensure we consider all nodes
		if mem, ok := memValues[nodeName]; ok && mem > maxMem {
			maxMem = mem
		}
		if cpu, ok := cpuValues[nodeName]; ok && cpu > maxCPU {
			maxCPU = cpu
		}
	}
	log.Printf("Max values for normalization - MemAvailable: %.0f bytes, CPUIdle(avg cores): %.3f", maxMem, maxCPU)

	nodeScores := make([]NodeScore, 0, len(compatibleNodes))
	for _, node := range compatibleNodes {
		memVal := 0.0
		if mv, ok := memValues[node.Name]; ok {
			memVal = mv
		}
		cpuVal := 0.0
		if cv, ok := cpuValues[node.Name]; ok {
			cpuVal = cv
		}

		memScore := normalizeScore(memVal, maxMem)
		cpuScore := normalizeScore(cpuVal, maxCPU)

		affinityScore, affErr := calculateAffinityScore(&node, pod)
		if affErr != nil {
			log.Printf("Warning: Failed to calculate affinity score for node %s: %v. Treating as 0.", node.Name, affErr)
			affinityScore = 0
		}

		totalScore := (config.WeightMem * memScore) + (config.WeightCPU * cpuScore) + (config.WeightAffinity * affinityScore)

		log.Printf("Scores for node %s: Mem=%.2f (Val=%.0f), CPU=%.2f (Val=%.3f), Affinity=%.2f -> Total=%.2f",
			node.Name, memScore, memVal, cpuScore, cpuVal, affinityScore, totalScore)

		nodeScores = append(nodeScores, NodeScore{
			Name:          node.Name,
			MemScore:      memScore,
			CPUScore:      cpuScore,
			AffinityScore: affinityScore,
			TotalScore:    totalScore,
		})
	}

	if len(nodeScores) == 0 {
		return "", errors.New("no nodes could be scored (potentially all metric fetches failed or no compatible nodes)")
	}

	sort.Slice(nodeScores, func(i, j int) bool {
		if nodeScores[i].TotalScore != nodeScores[j].TotalScore {
			return nodeScores[i].TotalScore > nodeScores[j].TotalScore
		}
		if nodeScores[i].MemScore != nodeScores[j].MemScore { // Tie-break on memory
			return nodeScores[i].MemScore > nodeScores[j].MemScore
		}
		return nodeScores[i].CPUScore > nodeScores[j].CPUScore // Then CPU
	})

	bestNode := nodeScores[0]
	log.Printf("Selected best node: %s (Total Score: %.2f, MemScore: %.2f, CPUScore: %.2f, AffinityScore: %.2f)",
		bestNode.Name, bestNode.TotalScore, bestNode.MemScore, bestNode.CPUScore, bestNode.AffinityScore)

	return bestNode.Name, nil
}
