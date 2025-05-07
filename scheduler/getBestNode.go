package main

import (
	"context"
	"fmt"
	"log"

	v1 "k8s.io/api/core/v1"
)

// getBestNode finds the v1.Node object corresponding to the best node name determined by scoring.
// It now calls the scoring logic from scoring.go
func getBestNode(ctx context.Context, compatibleNodes []v1.Node, pod *v1.Pod, config ScoringConfig) (v1.Node, error) {
	if len(compatibleNodes) == 0 {
		return v1.Node{}, fmt.Errorf("no compatible nodes provided to select from")
	}

	// Get name of best node per scoring logic
	bestNodeName, err := ScoreNodes(ctx, compatibleNodes, pod, config)
	if err != nil {
		// Error already logged in ScoreNodes
		return v1.Node{}, fmt.Errorf("node scoring failed: %w", err) // Return empty node struct on error
	}

	log.Printf("Best Node Name determined by scoring: %s", bestNodeName)

	// Find the actual node object from the compatible list
	for _, n := range compatibleNodes {
		if n.Name == bestNodeName {
			log.Printf("Found matching node object for %s", bestNodeName)
			return n, nil // Return the found node
		}
	}

	// This should ideally not happen if ScoreNodes logic is correct
	// and uses names present in compatibleNodes.
	log.Printf("Error: Unable to find Node object for determined best node name '%s' in the compatible list", bestNodeName)
	return v1.Node{}, fmt.Errorf("internal error: unable to find node object for name %s in compatible list", bestNodeName)
}
