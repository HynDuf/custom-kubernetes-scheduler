package main

import (
	"context" // Add context
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

// NOTE: The original reconcileUnscheduledPods function is less suitable for a client-go
// watch-based approach. The main loop now handles incoming pods directly from the watch.
// This function is kept for reference but is NOT CALLED in the updated main.go.
func reconcileUnscheduledPods(interval int, done chan struct{}, wg *sync.WaitGroup, clientset *kubernetes.Clientset, ctx context.Context) {
	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()
	defer wg.Done()

	for {
		select {
		case <-ticker.C:
			log.Println("Reconciliation loop triggered (Note: This is likely inactive in the current main.go)")
			// err := scheduleAllUnscheduled(ctx, clientset) // Call a function to list and schedule all
			// if err != nil {
			// 	log.Printf("Error during reconciliation: %v", err)
			// }
		case <-done:
			log.Println("Stopped reconciliation loop.")
			return
		}
	}
}

// monitorUnscheduledPods is now primarily handled by the watch loop in main.go
// This function signature is kept for reference but logic moved to main.go
func monitorUnscheduledPods(done chan struct{}, wg *sync.WaitGroup, clientset *kubernetes.Clientset, ctx context.Context) {
	// Original logic using watchUnscheduledPods is now integrated into main.go's select loop
	// This function body can be removed or adapted if a separate monitoring goroutine is desired.
	log.Println("monitorUnscheduledPods started (Note: Core logic moved to main.go)")
	<-done // Block until done signal
	wg.Done()
	log.Println("monitorUnscheduledPods stopped.")

}

// schedulePod takes an unscheduled pod, runs predicates, selects the best node, and binds it.
// This function is called by the main loop when a pod arrives on the channel.
func schedulePod(ctx context.Context, clientset *kubernetes.Clientset, pod *v1.Pod) error {
	processorLock.Lock() // Lock to prevent concurrent processing of the *same* pod if watch delivers duplicates quickly
	defer processorLock.Unlock()

	log.Printf("Attempting to schedule pod: %s/%s", pod.Namespace, pod.Name)

	// 1. Run Predicate Checks
	compatibleNodes, err := predicateChecks(ctx, clientset, pod)
	if err != nil {
		// Error logging and event posting are handled within predicateChecks
		return fmt.Errorf("predicate check failed: %w", err) // Return error to main loop
	}
	// predicateChecks returns an error if len(compatibleNodes) == 0

	// 2. Find Best Node (Prioritize) based on Prometheus Metric
	bestNode, err := getBestNode(ctx, compatibleNodes) // getBestNode now returns v1.Node
	if err != nil {
		log.Printf("Failed to get best node for pod %s/%s via Prometheus: %v", pod.Namespace, pod.Name, err)
		// --- Fallback Strategy ---
		log.Printf("Falling back to scheduling pod %s/%s on the first compatible node: %s", pod.Namespace, pod.Name, compatibleNodes[0].Name)
		bestNode = compatibleNodes[0] // Select the first compatible node
		// If failing is preferred:
		// _ = postEvent(ctx, clientset, pod, "FailedScheduling", fmt.Sprintf("Failed to find best node via metrics: %v", err), "Warning")
		// return fmt.Errorf("prioritization failed: %w", err)
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
// Kept for reference.
func scheduleAllUnscheduled(ctx context.Context, clientset *kubernetes.Clientset) error {
	processorLock.Lock() // Ensure only one reconciliation runs at a time
	defer processorLock.Unlock()

	log.Println("Running scheduleAllUnscheduled reconciliation...")
	// List unscheduled pods assigned to this scheduler
	podList, err := clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("spec.nodeName", "").String(),
	})
	if err != nil {
		return fmt.Errorf("failed to list pods for reconciliation: %w", err)
	}

	processedCount := 0
	for _, pod := range podList.Items {
		// Must check scheduler name again here
		if pod.Spec.SchedulerName == SchedulerName && pod.Status.Phase == v1.PodPending {
			log.Printf("Reconciler found unscheduled pod: %s/%s", pod.Namespace, pod.Name)
			// Use a detached context for each pod scheduling attempt within reconcile?
			// Or use the main context? Using main context for now.
			err := schedulePod(ctx, clientset, &pod) // Pass pointer to pod
			if err != nil {
				// Log error but continue processing other pods
				log.Printf("Error scheduling pod %s/%s during reconciliation: %v", pod.Namespace, pod.Name, err)
			} else {
				processedCount++
			}
		}
	}
	log.Printf("Reconciliation finished, processed %d pods.", processedCount)
	return nil
}
