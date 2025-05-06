package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
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

// Prometheus metric response structure (reused from getBestNodeName)
type MetricResponse struct {
	Status string `json:"status"`
	Data   Data   `json:"data"`
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
	prometheusService     = "http://prometheus-service.monitoring.svc.cluster.local:8080"
	memQueryTemplate      = "/api/v1/query?query=node_memory_MemAvailable_bytes"
	cpuIdleQueryTemplate  = "/api/v1/query?query=avg by (instance) (rate(node_cpu_seconds_total{mode=\"idle\"}[1m]))" // Avg Idle Cores
	maxScore         int = 10
	minScore         int = 0
)

// Function to query Prometheus
func queryPrometheus(ctx context.Context, query string) (*MetricResponse, error) {
	queryURL := prometheusService + query
	log.Printf("Querying Prometheus: %s", queryURL)

	req, err := http.NewRequestWithContext(ctx, "GET", queryURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create prometheus request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to query prometheus: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Attempt to read body for more info
		bodyBytes, _ := json.Marshal(resp.Body) // Read body even on error
		log.Printf("Prometheus returned non-OK status: %s. Body: %s", resp.Status, string(bodyBytes))
		return nil, fmt.Errorf("prometheus query failed with status: %s", resp.Status)
	}

	var metrics MetricResponse
	decoder := json.NewDecoder(resp.Body)
	err = decoder.Decode(&metrics)
	if err != nil {
		return nil, fmt.Errorf("failed to decode prometheus response: %w", err)
	}

	if metrics.Status != "success" {
		log.Printf("Prometheus query status was not 'success': %s", metrics.Status)
		return nil, fmt.Errorf("prometheus query status indicates failure: %s", metrics.Status)
	}

	log.Printf("Prometheus query '%s' returned %d results.", query, len(metrics.Data.Results))
	return &metrics, nil
}

// Parses the 'instance' or 'node' label from Prometheus result to get the k8s node name.
// Handles potential ports like ":9100".
func getNodeNameFromMetric(metricInfo map[string]string) (string, error) {
	instanceLabel, okInstance := metricInfo["instance"]
	nodeLabel, okNode := metricInfo["node"]

	var nodeNameFromMetric string
	rawLabel := ""

	if okInstance {
		rawLabel = instanceLabel
		nodeNameFromMetric = parseInstanceLabel(instanceLabel)
	} else if okNode {
		rawLabel = nodeLabel
		nodeNameFromMetric = parseInstanceLabel(nodeLabel) // Also parse node label in case it has port
	} else {
		return "", fmt.Errorf("metric result missing 'instance' or 'node' label: %v", metricInfo)
	}

	if nodeNameFromMetric == "" {
		return "", fmt.Errorf("could not determine node name from labels (raw: '%s'): %v", rawLabel, metricInfo)
	}
	return nodeNameFromMetric, nil
}

// Helper to parse instance/node label, removing port if present
func parseInstanceLabel(label string) string {
	if portIndex := strings.LastIndex(label, ":"); portIndex != -1 {
		// Simple check: if the part after colon looks like a number, assume it's a port
		if _, err := strconv.Atoi(label[portIndex+1:]); err == nil {
			return label[:portIndex] // Extract name before the last colon
		}
		// Else: Found a colon, but suffix isn't a number (IPv6?). Return full label.
	}
	// No colon found, assume the whole label is the node name
	return label
}

// Fetches Memory and CPU metrics for the given compatible nodes.
// Returns maps: nodeName -> value
func fetchNodeMetrics(ctx context.Context, compatibleNodeNames map[string]struct{}) (map[string]float64, map[string]float64, error) {
	memValues := make(map[string]float64)
	cpuValues := make(map[string]float64) // Idle cores

	// Fetch Memory
	memMetrics, err := queryPrometheus(ctx, memQueryTemplate)
	if err != nil {
		return nil, nil, fmt.Errorf("failed fetching memory metrics: %w", err)
	}
	for _, m := range memMetrics.Data.Results {
		nodeName, err := getNodeNameFromMetric(m.MetricInfo)
		if err != nil {
			log.Printf("Skipping memory metric: %v", err)
			continue
		}
		if _, isCompatible := compatibleNodeNames[nodeName]; !isCompatible {
			continue // Ignore nodes not in the compatible list
		}
		val, err := parseMetricValue(m.MetricValue)
		if err != nil {
			log.Printf("Skipping memory metric for node %s: %v", nodeName, err)
			continue
		}
		memValues[nodeName] = val
	}

	// Fetch CPU Idle
	cpuMetrics, err := queryPrometheus(ctx, cpuIdleQueryTemplate)
	if err != nil {
		return nil, nil, fmt.Errorf("failed fetching CPU metrics: %w", err)
	}
	for _, m := range cpuMetrics.Data.Results {
		nodeName, err := getNodeNameFromMetric(m.MetricInfo)
		if err != nil {
			log.Printf("Skipping cpu metric: %v", err)
			continue
		}
		if _, isCompatible := compatibleNodeNames[nodeName]; !isCompatible {
			continue // Ignore nodes not in the compatible list
		}
		val, err := parseMetricValue(m.MetricValue)
		if err != nil {
			log.Printf("Skipping cpu metric for node %s: %v", nodeName, err)
			continue
		}
		cpuValues[nodeName] = val
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
	if maxValue <= 0 || value <= 0 { // Avoid division by zero and negative scores
		return float64(minScore)
	}
	score := (value / maxValue) * float64(maxScore)
	if score < float64(minScore) {
		return float64(minScore)
	}
	if score > float64(maxScore) {
		return float64(maxScore)
	}
	return score
}

// Calculate the affinity score bonus for a node based on pod preferences.
// Score is the sum of weights of matching preferences.
func calculateAffinityScore(node *v1.Node, pod *v1.Pod) (float64, error) {
	affinity := pod.Spec.Affinity
	if affinity == nil || affinity.NodeAffinity == nil || affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution == nil {
		return 0, nil // No preference defined
	}

	var totalPreferenceScore int32 = 0
	nodeLabels := labels.Set(node.Labels) // Use labels.Set for matching

	for _, preferredTerm := range affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
		if preferredTerm.Preference.MatchExpressions == nil && preferredTerm.Preference.MatchFields == nil {
			continue // Skip empty preference term
		}

		// TODO: Implement MatchFields if needed (less common for preferred)

		// MatchExpressions logic
		if preferredTerm.Preference.MatchExpressions != nil {
			nodeSelector, err := NodeSelectorRequirementsAsSelector(preferredTerm.Preference.MatchExpressions)
			if err != nil {
				log.Printf("Warning: Failed to parse MatchExpressions for node %s, pod %s/%s: %v", node.Name, pod.Namespace, pod.Name, err)
				continue // Skip this term on error
			}

			if nodeSelector.Matches(nodeLabels) {
				log.Printf("Node %s matched preferred affinity term (weight %d) for pod %s/%s", node.Name, preferredTerm.Weight, pod.Namespace, pod.Name)
				totalPreferenceScore += preferredTerm.Weight
			}
		}
	}

	// Normalize the affinity score (0-10)?
	// Max possible score is sum of all weights (could be > 100).
	// Simple approach: scale linearly based on a max expected sum (e.g., 100).
	// Or just use the raw sum divided by 10 for rough alignment with 0-10 resource scores.
	scaledScore := float64(totalPreferenceScore) / 10.0 // Scale to roughly 0-10 range

	// Cap the score
	if scaledScore > float64(maxScore) {
		scaledScore = float64(maxScore)
	}

	log.Printf("Node %s affinity score (scaled 0-10): %.2f (raw sum: %d)", node.Name, scaledScore, totalPreferenceScore)
	return scaledScore, nil
}

// NodeSelectorRequirementsAsSelector converts the []NodeSelectorRequirement api type into a struct that implements
// labels.Selector. Helper function similar to metav1.LabelSelectorAsSelector
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

	// Create map for quick lookup
	compatibleNodeMap := make(map[string]struct{})
	for _, node := range compatibleNodes {
		compatibleNodeMap[node.Name] = struct{}{}
	}

	// Fetch metrics from Prometheus
	memValues, cpuValues, err := fetchNodeMetrics(ctx, compatibleNodeMap)
	if err != nil {
		log.Printf("Warning: Failed to fetch some node metrics: %v. Scoring may be incomplete.", err)
		// Decide whether to proceed with potentially missing data or fail
		// For now, proceed, nodes without metrics will get score 0
		// return "", fmt.Errorf("failed to fetch node metrics: %w", err)
	}

	// Find max values for normalization
	maxMem := 0.0
	maxCPU := 0.0
	for _, node := range compatibleNodes {
		if mem, ok := memValues[node.Name]; ok && mem > maxMem {
			maxMem = mem
		}
		if cpu, ok := cpuValues[node.Name]; ok && cpu > maxCPU {
			maxCPU = cpu
		}
	}
	log.Printf("Max values for normalization - MemAvailable: %.0f bytes, CPUIdle(avg cores): %.3f", maxMem, maxCPU)

	// Calculate scores for each node
	nodeScores := make([]NodeScore, 0, len(compatibleNodes))
	for _, node := range compatibleNodes {
		memVal := memValues[node.Name] // Defaults to 0 if not found
		cpuVal := cpuValues[node.Name] // Defaults to 0 if not found

		memScore := normalizeScore(memVal, maxMem)
		cpuScore := normalizeScore(cpuVal, maxCPU)

		affinityScore, err := calculateAffinityScore(&node, pod)
		if err != nil {
			// Log error but continue, treating affinity as 0 for this node
			log.Printf("Warning: Failed to calculate affinity score for node %s: %v", node.Name, err)
			affinityScore = 0
		}

		// Combine scores with weights
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
		// This might happen if metric fetching failed entirely and we chose to continue
		return "", errors.New("no nodes could be scored (check metric fetching logs)")
	}

	// Sort nodes by TotalScore descending
	sort.Slice(nodeScores, func(i, j int) bool {
		// Higher total score is better
		if nodeScores[i].TotalScore != nodeScores[j].TotalScore {
			return nodeScores[i].TotalScore > nodeScores[j].TotalScore
		}
		// Tie-breaking: prefer higher memory score
		if nodeScores[i].MemScore != nodeScores[j].MemScore {
			return nodeScores[i].MemScore > nodeScores[j].MemScore
		}
        // Tie-breaking: prefer higher CPU score
        return nodeScores[i].CPUScore > nodeScores[j].CPUScore
		// Could add more tie-breakers if needed
	})

	bestNode := nodeScores[0]
	log.Printf("Selected best node: %s (Total Score: %.2f)", bestNode.Name, bestNode.TotalScore)

	return bestNode.Name, nil
}
