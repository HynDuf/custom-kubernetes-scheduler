package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"reflect"
	"strconv"
	"sync"
	"syscall"
	"time" // Added for delays

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
)

// Ensure SchedulerName matches the one in kubernetes.go and YAMLs

// Global scoring configuration
var scoringConfig ScoringConfig

func loadScoringConfig() {
	// Load weights from environment variables with defaults
	scoringConfig.WeightMem = getEnvFloat("SCORE_WEIGHT_MEM", 0.5)       // Default 50%
	scoringConfig.WeightCPU = getEnvFloat("SCORE_WEIGHT_CPU", 0.5)       // Default 50%
	scoringConfig.WeightAffinity = getEnvFloat("SCORE_WEIGHT_AFFINITY", 1.0) // Default 1.0 (adjust as needed)

	// Normalize weights if they don't sum to roughly 1 (optional, depends on desired behavior)
	// totalWeight := scoringConfig.WeightMem + scoringConfig.WeightCPU // Only normalize resource weights?
	// if totalWeight > 0 && (totalWeight < 0.99 || totalWeight > 1.01) { // Allow slight float inaccuracy
	//  log.Printf("Normalizing resource weights (Mem: %.2f, CPU: %.2f)", scoringConfig.WeightMem, scoringConfig.WeightCPU)
	//  scoringConfig.WeightMem /= totalWeight
	//  scoringConfig.WeightCPU /= totalWeight
	// }

	log.Printf("Loaded Scoring Configuration: MemWeight=%.2f, CPUWeight=%.2f, AffinityWeight=%.2f",
		scoringConfig.WeightMem, scoringConfig.WeightCPU, scoringConfig.WeightAffinity)
}

// Helper to get environment variable as float64 or return default
func getEnvFloat(key string, fallback float64) float64 {
	if valueStr, ok := os.LookupEnv(key); ok {
		if valueFloat, err := strconv.ParseFloat(valueStr, 64); err == nil {
			return valueFloat
		} else {
			log.Printf("Warning: Invalid value for env var %s: '%s'. Using default %.2f. Error: %v", key, valueStr, fallback, err)
		}
	}
	log.Printf("Using default value for env var %s: %.2f", key, fallback)
	return fallback
}

func main() {
	log.Printf("Starting %s scheduler...", SchedulerName)

	// Load scoring configuration from environment variables
	loadScoringConfig()

	// Initialize Kubernetes client
	clientset, err := initKubernetesClient()
	if err != nil {
		log.Fatalf("Fatal: Failed to initialize Kubernetes client: %v", err)
	}
	log.Println("Kubernetes client initialized successfully.")

	// Create a context that can be cancelled for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // Ensure context is cancelled eventually

	var wg sync.WaitGroup // WaitGroup to wait for goroutines to finish

	// Set up signal handling
	stopCh := make(chan os.Signal, 1)
	signal.Notify(stopCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-stopCh
		log.Printf("Received shutdown signal: %v. Cancelling context...", sig)
		cancel() // Trigger shutdown for all goroutines using ctx
	}()

	informerFactory := informers.NewSharedInformerFactory(clientset, 0)

	// Node Informer
	nodeInformer := informerFactory.Core().V1().Nodes().Informer()
	nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldNode := oldObj.(*v1.Node)
			newNode := newObj.(*v1.Node)
			// Simple check: Trigger reschedule if labels or taints change
			// A more sophisticated check could compare specific conditions relevant to scheduling
			if !reflect.DeepEqual(oldNode.Labels, newNode.Labels) || !reflect.DeepEqual(oldNode.Spec.Taints, newNode.Spec.Taints) {
				log.Printf("Node %s changed (labels/taints), potentially triggering rescheduling...", newNode.Name)
				// TODO: Implement a mechanism to trigger rescheduling for affected pods if needed.
				// This is complex - might involve re-evaluating pods already on the node
				// or just re-queueing pending pods (simpler).
				scheduleAllUnscheduled(ctx, clientset, scoringConfig) // Example: Requeue all pending
			}
		},
        // AddFunc, DeleteFunc can also trigger rescheduling logic if needed
	})

	// Wating for cache sync
	go informerFactory.Start(ctx.Done())
    log.Println("Waiting for cache sync...")
	if !cache.WaitForCacheSync(ctx.Done(), nodeInformer.HasSynced) {
		log.Fatalf("Timed out waiting for node informer sync")
	}
	log.Println("Node informer synced.")


	// Start watching for unscheduled pods assigned to this scheduler
	podCh, errCh := watchUnscheduledPods(ctx, clientset)

	log.Println("Scheduler initialized. Waiting for pods...")

	// Main processing loop
	wg.Add(1) // Add 1 for the main processing loop goroutine
	go func() {
		defer wg.Done() // Signal completion when loop exits
		for {
			select {
			case <-ctx.Done():
				log.Println("Main processing loop stopping due to context cancellation.")
				return // Exit goroutine

			case err, ok := <-errCh:
				if !ok {
					log.Println("Error channel closed.")
					errCh = nil // Stop selecting on closed channel
					continue
				}
				log.Printf("Warning: Pod watch error: %v", err)

			case pod, ok := <-podCh:
				if !ok {
					log.Println("Pod channel closed. Scheduler likely shutting down.")
					if ctx.Err() == nil {
						log.Println("Error: Pod channel closed unexpectedly.")
					}
					podCh = nil // Stop selecting on closed channel
					continue
				}

				// Process the received pod
				log.Printf("Received pod %s/%s for scheduling.", pod.Namespace, pod.Name)
				// Pass the global scoringConfig
				err := schedulePod(ctx, clientset, &pod, scoringConfig)
				if err != nil {
					log.Printf("Failed to schedule pod %s/%s: %v", pod.Namespace, pod.Name, err)
					// SchedulePod logs details and posts events.
				}
				// Add a small delay to prevent tight loops if things go wrong
				time.Sleep(50 * time.Millisecond)
			}

			// Break loop if both channels are nil (closed)
			if errCh == nil && podCh == nil {
				log.Println("Both watch channels closed, exiting main processing loop.")
				return
			}
		}
	}()

	// Keep main goroutine alive until context is cancelled
	<-ctx.Done()

	log.Println("Waiting for main processing loop to finish...")
	wg.Wait() // Wait for the processing loop goroutine to exit
	log.Println("Scheduler shut down gracefully.")
}
