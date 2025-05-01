package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time" // Added for delays

	// "k8s.io/client-go/kubernetes" // Already imported via other files if needed standalone
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
)

// Ensure SchedulerName matches the one in kubernetes.go and YAMLs

func main() {
	log.Printf("Starting %s scheduler...", SchedulerName)

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
		// AddFunc:    scheduler.handleNodeAdd,
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldNode := oldObj.(*v1.Node)
			newNode := newObj.(*v1.Node)
			if needReschedule(oldNode, newNode) {
				log.Println("Node changed, requeuing pending pods...")
				scheduleAllUnscheduled(ctx, clientset) // Requeue all pending pods for scheduling
			}
		},
		// DeleteFunc: scheduler.handleNodeDelete,
	})

	// Wating for cache sync
	go informerFactory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), nodeInformer.HasSynced) {
		log.Fatalf("Timed out waiting for node informer sync")
	}

	log.Println("Node informer synced")

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
					// Consider if we should restart the watch or exit? Restarting is handled by watch loop.
					errCh = nil // Stop selecting on closed channel
					continue
				}
				// Log non-fatal errors from the watcher and continue
				log.Printf("Warning: Pod watch error: %v", err)
				// Optional: Implement backoff or specific error handling here

			case pod, ok := <-podCh:
				if !ok {
					log.Println("Pod channel closed. Scheduler likely shutting down.")
					// If context isn't cancelled, something unexpected happened.
					if ctx.Err() == nil {
						log.Println("Error: Pod channel closed unexpectedly.")
						// Optionally trigger cancellation or attempt restart
						// cancel()
					}
					podCh = nil // Stop selecting on closed channel
					continue
				}

				// Process the received pod
				// Run scheduling logic in a separate goroutine?
				// For simplicity, running sequentially for now.
				// Locking is handled in schedulePod if needed for pod-level concurrency.
				log.Printf("Received pod %s/%s for scheduling.", pod.Namespace, pod.Name)
				err := schedulePod(ctx, clientset, &pod) // Pass context
				if err != nil {
					log.Printf("Failed to schedule pod %s/%s: %v", pod.Namespace, pod.Name, err)
					// SchedulePod logs details and posts events. What else to do?
					// Maybe requeue after delay? Complicates logic significantly.
					// For now, just log and move on.
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
