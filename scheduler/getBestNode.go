package main

import (
	"context" // Add context
	"fmt"
	"log"

	v1 "k8s.io/api/core/v1" // Use official types
)

// getBestNode finds the v1.Node object corresponding to the best node name determined by metrics.
func getBestNode(ctx context.Context, compatibleNodes []v1.Node) (v1.Node, error) {
	// Get name of best node per metrics
	bestNodeName, err := getBestNodeName(ctx, compatibleNodes)
	if err != nil {
		// Error already logged in getBestNodeName
		return v1.Node{}, err // Return empty node struct on error
	}

	log.Printf("Best Node Name determined by metrics: %s", bestNodeName)

	// Find the actual node object from the compatible list
	for _, n := range compatibleNodes {
		if n.Name == bestNodeName {
			log.Printf("Found matching node object for %s", bestNodeName)
			return n, nil // Return the found node
		}
	}

	// This should ideally not happen if getBestNodeName logic is correct
	// and uses names present in compatibleNodes.
	log.Printf("Error: Unable to find Node object for determined best node name '%s' in the compatible list", bestNodeName)
	return v1.Node{}, fmt.Errorf("unable to find node object for name %s in compatible list", bestNodeName)
}
