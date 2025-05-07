package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	// Kubernetes client-go imports
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	// For parsing quantities like "500m" or "1Gi" correctly
	"k8s.io/apimachinery/pkg/api/resource"
)

// SchedulerName identifies the custom scheduler. Pods must have spec.schedulerName == SchedulerName to be handled.
const SchedulerName = "custom-scheduler" // Match this with YAMLs and main.go const

// initKubernetesClient creates a Kubernetes clientset using in-cluster configuration.
func initKubernetesClient() (*kubernetes.Clientset, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get in-cluster config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create clientset: %w", err)
	}
	return clientset, nil
}

// watchUnscheduledPods watches for pods that are unscheduled and assigned to our custom scheduler.
func watchUnscheduledPods(ctx context.Context, clientset *kubernetes.Clientset) (<-chan v1.Pod, <-chan error) {
	podChannel := make(chan v1.Pod)
	errChannel := make(chan error, 1) // Buffered channel for non-blocking error send

	go func() {
		defer close(podChannel)
		defer close(errChannel) // Ensure error channel is closed on exit

		for {
			select {
			case <-ctx.Done(): // Handle context cancellation
				log.Println("Watch context cancelled, stopping pod watch.")
				return
			default:
				log.Println("Starting watch for unscheduled pods...")
				// Watch pods in all namespaces, filter by spec.nodeName="" (unscheduled)
				watcher, err := clientset.CoreV1().Pods("").Watch(ctx, metav1.ListOptions{
					FieldSelector: fields.OneTermEqualSelector("spec.nodeName", "").String(),
				})

				if err != nil {
					log.Printf("Error starting pod watch: %v. Retrying in 5 seconds...", err)
					select {
					case errChannel <- fmt.Errorf("failed to start watch: %w", err): // Try sending non-fatal error
					default:
						log.Println("Error channel full, discarding watch start error.")
					}
					select {
					case <-time.After(5 * time.Second):
						continue // Retry watch
					case <-ctx.Done():
						log.Println("Watch context cancelled during retry wait.")
						return
					}
				}

				log.Println("Pod watch started successfully.")
			processEvents:
				for {
					select {
					case <-ctx.Done():
						log.Println("Watch context cancelled, stopping event processing.")
						watcher.Stop() // Explicitly stop the watcher
						return
					case event, ok := <-watcher.ResultChan():
						if !ok {
							log.Println("Pod watch channel closed. Restarting watch...")
							break processEvents // Break inner loop to restart watch
						}

						switch event.Type {
						case watch.Added, watch.Modified: // Handle Modified too, in case schedulerName is added later
							pod, ok := event.Object.(*v1.Pod)
							if !ok {
								log.Printf("Watch event object is not a Pod: %T", event.Object)
								continue
							}
							// Check if the pod is assigned to *this* scheduler, is Pending, and unscheduled
							if pod.Spec.SchedulerName == SchedulerName && pod.Spec.NodeName == "" && pod.Status.Phase == v1.PodPending {
								log.Printf("Found unscheduled pod for %s: %s/%s (Event: %s)", SchedulerName, pod.Namespace, pod.Name, event.Type)
								select {
								case podChannel <- *pod: // Send a copy to the channel
								case <-ctx.Done():
									log.Println("Context cancelled while sending pod.")
									watcher.Stop()
									return
								}
							}
						case watch.Error:
							status, ok := event.Object.(*metav1.Status)
							errMsg := "unknown watch error"
							if ok {
								errMsg = status.Message
								log.Printf("Error during pod watch: %s (Code: %d)", errMsg, status.Code)
								// Handle specific errors like "too old resource version" which requires restarting the watch
								if status.Reason == metav1.StatusReasonGone || status.Code == http.StatusGone {
                                    log.Println("Watch resource version too old, restarting watch immediately.")
                                    watcher.Stop() // Stop current watcher
                                    break processEvents // Restart watch loop
                                }
							} else {
								log.Printf("Received unexpected error object during watch: %T", event.Object)
								errMsg = "received unexpected error object during watch"
							}
							select {
							case errChannel <- fmt.Errorf("watch error: %s", errMsg):
							default:
								log.Println("Error channel full, discarding watch error.")
							}
							// Watcher might close after error, outer loop will restart if not handled above
							break processEvents

						case watch.Deleted:
							pod, ok := event.Object.(*v1.Pod)
							if ok && pod.Spec.SchedulerName == SchedulerName && pod.Spec.NodeName == "" {
								log.Printf("Unscheduled pod %s/%s deleted.", pod.Namespace, pod.Name)
							}
							// Ignore Bookmark etc.
						}
					}
				}
				// Brief pause before retrying after watch channel closes normally
				select {
				case <-time.After(1 * time.Second):
				case <-ctx.Done():
					log.Println("Watch context cancelled during restart delay.")
					return
				}
			}
		}
	}()

	return podChannel, errChannel
}

// Checks if ANY toleration tolerates the taint
func tolerationsTolerateTaint(tolerations []v1.Toleration, taint *v1.Taint) bool {
	for i := range tolerations {
		if tolerations[i].ToleratesTaint(taint) {
			return true
		}
	}
	return false
}

// Checks if ALL taints are tolerated by at least one toleration
func tolerationsTolerateTaints(tolerations []v1.Toleration, taints []v1.Taint) bool {
	for i := range taints {
		if !tolerationsTolerateTaint(tolerations, &taints[i]) {
			return false
		}
	}
	return true
}

// needReschedule checks if the node's taints have changed
// and returns true if rescheduling is needed.
func needReschedule(oldNode, newNode *v1.Node) bool {
	// Check if the node's taints have changed
	if len(oldNode.Spec.Taints) != len(newNode.Spec.Taints) {
		return true // Taints changed, reschedule needed
	}

	// More robust check needed here - order might change
	oldTaints := make(map[string]v1.Taint)
	for _, t := range oldNode.Spec.Taints {
		oldTaints[t.Key+string(t.Effect)] = t // Use Key+Effect as identifier
	}
	newTaints := make(map[string]v1.Taint)
	for _, t := range newNode.Spec.Taints {
		newTaints[t.Key+string(t.Effect)] = t
	}

	if len(oldTaints) != len(newTaints) { // Should be caught by len check above, but safer
		return true
	}

	for key, oldTaint := range oldTaints {
		newTaint, ok := newTaints[key]
		if !ok || oldTaint.Value != newTaint.Value { // Check if taint exists and value matches
			return true
		}
	}

	// Also check labels relevant to affinity rules? More complex.
	// For now, just checking taints.

	return false // No relevant changes detected
}

// predicateChecks finds nodes that are suitable for the pod based on resource requests, selectors, taints etc.
// It returns a list of nodes that pass all checks, including those with PreferNoSchedule taints.
// The scoring phase will handle the preference.
func predicateChecks(ctx context.Context, clientset *kubernetes.Clientset, podToSchedule *v1.Pod) ([]v1.Node, error) {
	log.Printf("Running predicate checks for pod: %s/%s", podToSchedule.Namespace, podToSchedule.Name)

	// 1. List potential nodes (filtering by nodeName and nodeSelector if specified)
	listOptions := metav1.ListOptions{}
	if podToSchedule.Spec.NodeName != "" {
		listOptions.FieldSelector = fields.OneTermEqualSelector("metadata.name", podToSchedule.Spec.NodeName).String()
	}
	if len(podToSchedule.Spec.NodeSelector) > 0 {
		selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: podToSchedule.Spec.NodeSelector})
		if err != nil {
			return nil, fmt.Errorf("invalid node selector: %w", err)
		}
		listOptions.LabelSelector = selector.String()
	}

	nodeList, err := clientset.CoreV1().Nodes().List(ctx, listOptions)
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}
	if len(nodeList.Items) == 0 {
        errMsg := "no nodes found matching NodeName/NodeSelector"
        if podToSchedule.Spec.NodeName != "" {
             errMsg += fmt.Sprintf(" (NodeName: %s)", podToSchedule.Spec.NodeName)
        }
         if len(podToSchedule.Spec.NodeSelector) > 0 {
              errMsg += fmt.Sprintf(" (NodeSelector: %v)", podToSchedule.Spec.NodeSelector)
         }
		_ = postEvent(ctx, clientset, podToSchedule, "FailedScheduling", errMsg, "Warning")
		return nil, errors.New(errMsg)
	}

	// 2. Get all pods (needed to calculate current usage on nodes)
	// TODO: Consider optimizing this if cluster is huge. Maybe only list pods relevant to candidate nodes?
	allPodsList, err := clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods for resource calculation: %w", err)
	}

	// 3. Calculate current resource usage per node (REQUESTS of running/pending pods)
	nodeUsage := calculateNodeUsage(nodeList.Items, allPodsList.Items)

	// 4. Calculate resources required by the new pod
	podRequiredCpu, podRequiredMemory := calculatePodResourceRequests(podToSchedule)
	log.Printf("Pod %s/%s requires: CPU=%s, Memory=%s", podToSchedule.Namespace, podToSchedule.Name, podRequiredCpu.String(), podRequiredMemory.String())

	// 5. Filter nodes based on predicates
	eligibleNodes := make([]v1.Node, 0, len(nodeList.Items)) // Includes strictly compatible + prefer-not-schedule
	fitFailures := make(map[string][]string)

	for _, node := range nodeList.Items {
		reasons := []string{}
		isStrictlyFit := true // Assume fit unless a hard requirement fails

		// Check: Node Unschedulable
		if node.Spec.Unschedulable {
			reasons = append(reasons, "Node is marked unschedulable")
			isStrictlyFit = false
		}

		// Check: Resource Availability
		if isStrictlyFit { // Only check resources if other hard checks pass
			cpuFit, memFit, reason := checkNodeResources(node, nodeUsage[node.Name], podRequiredCpu, podRequiredMemory)
			if !cpuFit || !memFit {
				reasons = append(reasons, reason)
				isStrictlyFit = false
			}
		}

		// Check: Taints and Tolerations
		if isStrictlyFit {
			passes, reason := checkNodeTaints(node, podToSchedule.Spec.Tolerations)
			if !passes {
				reasons = append(reasons, reason)
				isStrictlyFit = false
			}
		}

		// Check Required Node Affinity/Anti-Affinity (Ignoring Preferred for now - handled in scoring)
		// TODO: Implement requiredDuringSchedulingIgnoredDuringExecution checks if needed

		if isStrictlyFit {
			log.Printf("Node %s PASSED predicate checks.", node.Name)
			eligibleNodes = append(eligibleNodes, node)
		} else {
			log.Printf("Node %s FAILED predicate checks: %s", node.Name, strings.Join(reasons, "; "))
			fitFailures[node.Name] = reasons
		}
	}

	// 6. Handle results
	if len(eligibleNodes) == 0 {
		log.Printf("Pod %s/%s failed to fit on any node.", podToSchedule.Namespace, podToSchedule.Name)
		failureMsg := fmt.Sprintf("pod (%s/%s) failed predicate checks on all nodes.", podToSchedule.Namespace, podToSchedule.Name)
		// Optional: Add details from fitFailures
		_ = postEvent(ctx, clientset, podToSchedule, "FailedScheduling", failureMsg, "Warning")
		return nil, errors.New("no eligible nodes found after predicate checks")
	}

	log.Printf("Found %d eligible nodes for pod %s/%s (will be scored).", len(eligibleNodes), podToSchedule.Namespace, podToSchedule.Name)
	return eligibleNodes, nil
}

// Helper: Calculate usage based on pod requests
func calculateNodeUsage(nodes []v1.Node, pods []v1.Pod) map[string]v1.ResourceList {
	nodeUsage := make(map[string]v1.ResourceList)
	for _, node := range nodes {
		nodeUsage[node.Name] = v1.ResourceList{
			v1.ResourceCPU:    *resource.NewQuantity(0, resource.DecimalSI),
			v1.ResourceMemory: *resource.NewQuantity(0, resource.BinarySI),
		}
	}

	for _, existingPod := range pods {
		if existingPod.Spec.NodeName == "" || existingPod.Status.Phase == v1.PodFailed || existingPod.Status.Phase == v1.PodSucceeded {
			continue
		}
		usage, nodeExists := nodeUsage[existingPod.Spec.NodeName]
		if !nodeExists {
			continue // Pod on a node not in our candidate list
		}

		for _, container := range existingPod.Spec.Containers {
			if cpuReq, ok := container.Resources.Requests[v1.ResourceCPU]; ok {
				currentCpu := usage[v1.ResourceCPU]
				currentCpu.Add(cpuReq)
				usage[v1.ResourceCPU] = currentCpu
			}
			if memReq, ok := container.Resources.Requests[v1.ResourceMemory]; ok {
				currentMem := usage[v1.ResourceMemory]
				currentMem.Add(memReq)
				usage[v1.ResourceMemory] = currentMem
			}
		}
		nodeUsage[existingPod.Spec.NodeName] = usage
	}
	return nodeUsage
}

// Helper: Calculate total pod resource requests
func calculatePodResourceRequests(pod *v1.Pod) (*resource.Quantity, *resource.Quantity) {
	podRequiredCpu := resource.NewQuantity(0, resource.DecimalSI)
	podRequiredMemory := resource.NewQuantity(0, resource.BinarySI)
	for _, container := range pod.Spec.Containers {
		if cpuReq, ok := container.Resources.Requests[v1.ResourceCPU]; ok {
			podRequiredCpu.Add(cpuReq)
		}
		if memReq, ok := container.Resources.Requests[v1.ResourceMemory]; ok {
			podRequiredMemory.Add(memReq)
		}
	}
	return podRequiredCpu, podRequiredMemory
}

// Helper: Check node resource availability against pod requests
func checkNodeResources(node v1.Node, currentUsage v1.ResourceList, podCPU, podMem *resource.Quantity) (bool, bool, string) {
	allocatable := node.Status.Allocatable
	cpuFit, memFit := true, true
	reasons := []string{}

	// Check CPU
	allocatableCpu := allocatable.Cpu()
	currentCpuUsage := currentUsage.Cpu()
	if allocatableCpu == nil {
		cpuFit = false
		reasons = append(reasons, "No allocatable CPU info")
	} else {
		availableCpu := allocatableCpu.DeepCopy()
		availableCpu.Sub(*currentCpuUsage)
		if availableCpu.Cmp(*podCPU) < 0 { // Use < 0 for comparison
			cpuFit = false
			reasons = append(reasons, fmt.Sprintf("Insufficient CPU (Req: %s, Avail: %s)", podCPU.String(), availableCpu.String()))
		}
	}

	// Check Memory
	allocatableMem := allocatable.Memory()
	currentMemUsage := currentUsage.Memory()
	if allocatableMem == nil {
		memFit = false
		reasons = append(reasons, "No allocatable Memory info")
	} else {
		availableMem := allocatableMem.DeepCopy()
		availableMem.Sub(*currentMemUsage)
		if availableMem.Cmp(*podMem) < 0 { // Use < 0 for comparison
			memFit = false
			reasons = append(reasons, fmt.Sprintf("Insufficient Memory (Req: %s, Avail: %s)", podMem.String(), availableMem.String()))
		}
	}

	return cpuFit, memFit, strings.Join(reasons, "; ")
}

// Helper: Check if pod tolerates node taints (excluding PreferNoSchedule)
func checkNodeTaints(node v1.Node, tolerations []v1.Toleration) (bool, string) {
	for _, taint := range node.Spec.Taints {
		// Ignore PreferNoSchedule taints in predicates; they are handled in scoring.
		if taint.Effect == v1.TaintEffectPreferNoSchedule {
			continue
		}
		// Check if the pod tolerates this specific taint (NoSchedule, NoExecute)
		if !tolerationsTolerateTaint(tolerations, &taint) {
			return false, fmt.Sprintf("Pod does not tolerate taint %s", taint.ToString())
		}
	}
	return true, "" // Pod tolerates all NoSchedule/NoExecute taints
}

// bindPod assigns the pod to the chosen node by creating a Binding object.
func bindPod(ctx context.Context, clientset *kubernetes.Clientset, podToSchedule *v1.Pod, nodeName string) error {
	log.Printf("Attempting to bind pod %s/%s to node %s", podToSchedule.Namespace, podToSchedule.Name, nodeName)

	binding := &v1.Binding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podToSchedule.Name,
			Namespace: podToSchedule.Namespace,
			Annotations: map[string]string{ // Add annotation indicating custom scheduler bound it
				"kubernetes.io/custom-scheduler": SchedulerName,
			},
		},
		Target: v1.ObjectReference{
			APIVersion: "v1",
			Kind:       "Node",
			Name:       nodeName,
		},
	}

	err := clientset.CoreV1().Pods(podToSchedule.Namespace).Bind(ctx, binding, metav1.CreateOptions{})
	if err != nil {
		log.Printf("Failed to bind pod %s/%s to node %s: %v", podToSchedule.Namespace, podToSchedule.Name, nodeName, err)
		_ = postEvent(ctx, clientset, podToSchedule, "FailedBinding", fmt.Sprintf("Error binding pod to node %s: %v", nodeName, err), "Warning") // Ignore event posting error
		return fmt.Errorf("failed to bind pod: %w", err)
	}

	log.Printf("Successfully bound pod %s/%s to node %s", podToSchedule.Namespace, podToSchedule.Name, nodeName)
	message := fmt.Sprintf("Successfully assigned %s/%s to %s by %s", podToSchedule.Namespace, podToSchedule.Name, nodeName, SchedulerName)
	_ = postEvent(ctx, clientset, podToSchedule, "Scheduled", message, "Normal") // Ignore event posting error
	return nil
}

// postEvent creates a Kubernetes event related to the pod scheduling.
func postEvent(ctx context.Context, clientset *kubernetes.Clientset, pod *v1.Pod, reason, message, eventType string) error {
	timestamp := metav1.Now()
	event := &v1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: pod.Name + "-", // Event names should be unique
			Namespace:    pod.Namespace,
		},
		InvolvedObject: v1.ObjectReference{
			Kind:            "Pod",
			Namespace:       pod.Namespace,
			Name:            pod.Name,
			UID:             pod.UID,
			APIVersion:      "v1",
			ResourceVersion: pod.ResourceVersion, // Use pod's ResourceVersion for consistency
		},
		Reason:  reason,
		Message: message,
		Source: v1.EventSource{
			Component: SchedulerName,
		},
		FirstTimestamp: timestamp,
		LastTimestamp:  timestamp,
		Count:          1,
		Type:           eventType, // "Normal" or "Warning"
		// Reporting fields help event correlation
		ReportingController: SchedulerName,
		ReportingInstance:   os.Getenv("POD_NAME"), // Get scheduler pod name if available
	}

	// Use a context with timeout for event creation
	eventCtx, cancel := context.WithTimeout(ctx, 10*time.Second) // 10 sec timeout for event creation
	defer cancel()

	_, err := clientset.CoreV1().Events(pod.Namespace).Create(eventCtx, event, metav1.CreateOptions{})
	if err != nil {
		// Log error if event creation fails (might happen under high load or network issues)
		log.Printf("Warning: Failed to create event (Reason: %s, Pod: %s/%s): %v", reason, pod.Namespace, pod.Name, err)
	}
	return err // Return error, but caller might ignore it
}
