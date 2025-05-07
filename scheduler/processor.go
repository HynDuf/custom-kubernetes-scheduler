package main

import (
	"context" // Add context
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"                       // Use official types
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1" // <-- Add this line
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
)

// processorLock prevents multiple goroutines from trying to schedule the same pod simultaneously.
// Note: The watch-based approach in main.go makes this less critical than the original reconcile loop.
var processorLock = &sync.Mutex{}

// NOTE: reconcileUnscheduledPods and monitorUnscheduledPods are kept for reference but are NOT CALLED in the updated main.go.
func reconcileUnscheduledPods(interval int, done chan struct{}, wg *sync.WaitGroup, clientset *kubernetes.Clientset, ctx context.Context, config ScoringConfig) { // Added config
	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()
	defer wg.Done()

	for {
		select {
		case <-ticker.C:
			log.Println("Reconciliation loop triggered (Note: This is likely inactive in the current main.go)")
			err := scheduleAllUnscheduled(ctx, clientset, config) // Pass config
			if err != nil {
				log.Printf("Error during reconciliation: %v", err)
			}
		case <-done:
			log.Println("Stopped reconciliation loop.")
			return
		}
	}
}

func monitorUnscheduledPods(done chan struct{}, wg *sync.WaitGroup, clientset *kubernetes.Clientset, ctx context.Context) {
	log.Println("monitorUnscheduledPods started (Note: Core logic moved to main.go)")
	<-done // Block until done signal
	wg.Done()
	log.Println("monitorUnscheduledPods stopped.")

}

// schedulePod takes an unscheduled pod, runs predicates, selects the best node via scoring, and binds it.
func schedulePod(ctx context.Context, clientset *kubernetes.Clientset, pod *v1.Pod, config ScoringConfig) error { // Added config
	processorLock.Lock()
	defer processorLock.Unlock()

	log.Printf("Attempting to schedule pod: %s/%s", pod.Namespace, pod.Name)

	// 1. Run Predicate Checks
	// predicateChecks now correctly handles PreferNoSchedule taints, returning them
	// if no strictly compatible nodes exist. Scoring will handle the preference.
	compatibleNodes, err := predicateChecks(ctx, clientset, pod)
	if err != nil {
		// Error logging and event posting are handled within predicateChecks
		return fmt.Errorf("predicate check failed: %w", err) // Return error to main loop
	}

	// 2. Find Best Node (Prioritize) using the new scoring logic
	bestNode, err := getBestNode(ctx, compatibleNodes, pod, config) // Pass pod and config
	if err != nil {
		log.Printf("Failed to get best node for pod %s/%s via scoring: %v", pod.Namespace, pod.Name, err)
		// --- Fallback Strategy ---
		// Check if compatibleNodes has anything before trying to access [0]
		if len(compatibleNodes) > 0 {
			log.Printf("Falling back to scheduling pod %s/%s on the first compatible/unpreferred node: %s", pod.Namespace, pod.Name, compatibleNodes[0].Name)
			bestNode = compatibleNodes[0] // Select the first compatible/unpreferred node
		} else {
			// This case should theoretically be caught by predicateChecks returning an error
			// if no nodes (compatible or unpreferred) are found.
			errMsg := "Scoring failed and no fallback node available (predicate checks likely failed initially)"
			_ = postEvent(ctx, clientset, pod, "FailedScheduling", errMsg, "Warning")
			return errors.New(errMsg)
		}
		// Original code:
		// log.Printf("Failed to get best node for pod %s/%s via Prometheus: %v", pod.Namespace, pod.Name, err)
		// log.Printf("Falling back to scheduling pod %s/%s on the first compatible node: %s", pod.Namespace, pod.Name, compatibleNodes[0].Name)
		// bestNode = compatibleNodes[0]
	}

	// 3. Bind Pod to the Chosen Node
	err = bindPod(ctx, clientset, pod, bestNode.Name) // Pass bestNode.Name
	if err != nil {
		// Error logging and event posting handled within bindPod
		return fmt.Errorf("binding failed: %w", err) // Return error to main loop
	}

	log.Printf("Successfully processed scheduling for pod %s/%s on node %s", pod.Namespace, pod.Name, bestNode.Name)
	return nil // Success
}

// scheduleAllUnscheduled is used by the (now likely inactive) reconcile loop.
// Kept for reference. Needs config passed.
func scheduleAllUnscheduled(ctx context.Context, clientset *kubernetes.Clientset, config ScoringConfig) error { // Added config
	log.Println("Running scheduleAllUnscheduled reconciliation...")
	podList, err := clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("spec.nodeName", "").String(),
	})
	if err != nil {
		return fmt.Errorf("failed to list pods for reconciliation: %w", err)
	}

	processedCount := 0
	for _, pod := range podList.Items {
		if pod.Spec.SchedulerName == SchedulerName && pod.Status.Phase == v1.PodPending {
			log.Printf("Reconciler found unscheduled pod: %s/%s", pod.Namespace, pod.Name)
			// Need to handle potential concurrent modification if called often
			// Use a copy of the pod object for scheduling attempt
			podToSchedule := pod.DeepCopy()
			err := schedulePod(ctx, clientset, podToSchedule, config) // Pass config and copied pod
			if err != nil {
				log.Printf("Error scheduling pod %s/%s during reconciliation: %v", pod.Namespace, pod.Name, err)
			} else {
				processedCount++
			}
		}
	}
	log.Printf("Reconciliation finished, processed %d pods.", processedCount)
	return nil
}
