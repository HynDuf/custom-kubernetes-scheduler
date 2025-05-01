package main

import (
	"context"
	"errors"
	"fmt"
	"log"
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
					// Optional: Add LabelSelector if pods for this scheduler have specific labels
				})

				if err != nil {
					log.Printf("Error starting pod watch: %v. Retrying in 5 seconds...", err)
					select {
					case errChannel <- fmt.Errorf("failed to start watch: %w", err): // Try sending non-fatal error
					default: // Avoid blocking if channel is full
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
				// Process events from the watcher channel
			processEvents:
				for {
					select {
					case <-ctx.Done():
						log.Println("Watch context cancelled, stopping event processing.")
						watcher.Stop() // Explicitly stop the watcher
						return
					case event, ok := <-watcher.ResultChan():
						if !ok {
							// Watch channel closed, restart watch
							log.Println("Pod watch channel closed. Restarting watch...")
							break processEvents // Break inner loop to restart watch
						}

						switch event.Type {
						case watch.Added:
							pod, ok := event.Object.(*v1.Pod)
							if !ok {
								log.Printf("Watch event object is not a Pod: %T", event.Object)
								continue
							}
							// Check if the pod is assigned to *this* scheduler and is Pending
							if pod.Spec.SchedulerName == SchedulerName && pod.Status.Phase == v1.PodPending {
								log.Printf("Found unscheduled pod for %s: %s/%s", SchedulerName, pod.Namespace, pod.Name)
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
								log.Printf("Error during pod watch: %s", errMsg)
							} else {
								log.Printf("Received unexpected error object during watch: %T", event.Object)
								errMsg = "received unexpected error object during watch"
							}
							select {
							case errChannel <- fmt.Errorf("watch error: %s", errMsg):
							default:
								log.Println("Error channel full, discarding watch error.")
							}
							// Watcher might close after error, outer loop will restart
							break processEvents // Break inner loop to restart watch

						case watch.Deleted:
							pod, ok := event.Object.(*v1.Pod)
							if ok && pod.Spec.SchedulerName == SchedulerName {
								log.Printf("Unscheduled pod %s/%s deleted.", pod.Namespace, pod.Name)
							}
							// Ignore Modified, Bookmark etc. for unscheduled pods
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

func tolerationsTolerateTaint(tolerations []v1.Toleration, taint *v1.Taint) bool {
	for i := range tolerations {
		if tolerations[i].ToleratesTaint(taint) {
			return true
		}
	}
	return false
}

func tolerationsTolerateTaints(tolerations []v1.Toleration, taints []v1.Taint) bool {
	for i := range taints {
		if !tolerationsTolerateTaint(tolerations, &taints[i]) {
			return false
		}
	}
	return true
}

func isSoftTaint(taint v1.Taint) bool {
	// Check if the taint is a soft taint (e.g., NoSchedule or PreferNoSchedule)
	return taint.Effect == v1.TaintEffectPreferNoSchedule
}

// predicateChecks finds nodes that are suitable for the pod based on resource requests.
func predicateChecks(ctx context.Context, clientset *kubernetes.Clientset, podToSchedule *v1.Pod) ([]v1.Node, error) {
	log.Printf("Running predicate checks for pod: %s/%s", podToSchedule.Namespace, podToSchedule.Name)

	// 1. Get all schedulable nodes (consider Unschedulable field)

	// Prepare label selector string if node selectors are present
	var labelSelector string
	if len(podToSchedule.Spec.NodeSelector) > 0 {
		selector, selectorErr := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
			MatchLabels: podToSchedule.Spec.NodeSelector,
		})
		if selectorErr != nil {
			return nil, fmt.Errorf("invalid node selector: %w", selectorErr)
		}
		labelSelector = selector.String()
	}

	nodeList, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		// node name (if specified)
		FieldSelector: func() string {
			if podToSchedule.Spec.NodeName != "" {
				return fields.OneTermEqualSelector("metadata.name", podToSchedule.Spec.NodeName).String()
			}
			return ""
		}(),
		LabelSelector: labelSelector,
	})

	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	// comment out because taints should not be filtered out here
	// Filter nodes based on tolerations
	// Taints and tolerations are handled in the predicate checks below
	// filteredNodes := []v1.Node{}
	// for _, node := range nodeList.Items {
	// 	if tolerationsTolerateTaints(podToSchedule.Spec.Tolerations, node.Spec.Taints) {
	// 		filteredNodes = append(filteredNodes, node)
	// 	}
	// }
	// nodeList.Items = filteredNodes

	if len(nodeList.Items) == 0 {
		return nil, errors.New("no nodes found in the cluster")
	}

	// 2. Get all pods (needed to calculate current usage on nodes)
	// Consider using ResourceVersion for consistency if needed, but adds complexity
	podList, err := clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	// 3. Calculate current resource usage per node (REQUESTS of running/pending pods)
	nodeUsage := make(map[string]v1.ResourceList)
	for _, node := range nodeList.Items {
		nodeUsage[node.Name] = v1.ResourceList{
			v1.ResourceCPU:    *resource.NewQuantity(0, resource.DecimalSI),
			v1.ResourceMemory: *resource.NewQuantity(0, resource.BinarySI),
		}
	}

	for _, existingPod := range podList.Items {
		// Only consider pods assigned to a node and not in a terminal state
		if existingPod.Spec.NodeName == "" || existingPod.Status.Phase == v1.PodFailed || existingPod.Status.Phase == v1.PodSucceeded {
			continue
		}
		usage, nodeExists := nodeUsage[existingPod.Spec.NodeName]
		if !nodeExists {
			continue // Pod running on a node not in our initial list (e.g., marked unschedulable later)
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

	// 4. Calculate resources required by the new pod
	podRequiredCpu := resource.NewQuantity(0, resource.DecimalSI)
	podRequiredMemory := resource.NewQuantity(0, resource.BinarySI)
	for _, container := range podToSchedule.Spec.Containers {
		if cpuReq, ok := container.Resources.Requests[v1.ResourceCPU]; ok {
			podRequiredCpu.Add(cpuReq)
		}
		if memReq, ok := container.Resources.Requests[v1.ResourceMemory]; ok {
			podRequiredMemory.Add(memReq)
		}
	}
	log.Printf("Pod %s/%s requires: CPU=%s, Memory=%s", podToSchedule.Namespace, podToSchedule.Name, podRequiredCpu.String(), podRequiredMemory.String())

	// 5. Filter nodes based on available resources and other predicates
	unPreferredNodes := make([]v1.Node, 0, len(nodeList.Items))
	compatibleNodes := make([]v1.Node, 0, len(nodeList.Items))
	fitFailures := make(map[string][]string)

	for _, node := range nodeList.Items {
		// Basic check: Is node marked unschedulable?
		if node.Spec.Unschedulable {
			fitFailures[node.Name] = append(fitFailures[node.Name], "Node is marked unschedulable")
			continue // Skip this node early
		}

		reasons := []string{}
		allocatable := node.Status.Allocatable
		currentUsage := nodeUsage[node.Name]

		// Check CPU
		allocatableCpu := allocatable.Cpu()
		currentCpuUsage := currentUsage.Cpu()
		if allocatableCpu == nil {
			reasons = append(reasons, "No allocatable CPU info")
		} else {
			availableCpu := allocatableCpu.DeepCopy()
			availableCpu.Sub(*currentCpuUsage)
			if availableCpu.Cmp(*podRequiredCpu) == -1 {
				reasons = append(reasons, fmt.Sprintf("Insufficient CPU (Req: %s, Avail: %s)", podRequiredCpu.String(), availableCpu.String()))
			}
		}

		// Check Memory
		allocatableMem := allocatable.Memory()
		currentMemUsage := currentUsage.Memory()
		if allocatableMem == nil {
			reasons = append(reasons, "No allocatable Memory info")
		} else {
			availableMem := allocatableMem.DeepCopy()
			availableMem.Sub(*currentMemUsage)
			if availableMem.Cmp(*podRequiredMemory) == -1 {
				reasons = append(reasons, fmt.Sprintf("Insufficient Memory (Req: %s, Avail: %s)", podRequiredMemory.String(), availableMem.String()))
			}
		}

		// Taint checks
		// Check if the node has any taints that the pod cannot tolerate
		isNoPrefered := false

		for _, taint := range node.Spec.Taints {
			if !tolerationsTolerateTaint(podToSchedule.Spec.Tolerations, &taint) {
				if isSoftTaint(taint) {
					isNoPrefered = true
				} else {
					reasons = append(reasons, fmt.Sprintf("Pod does not tolerate taint %s", taint.ToString()))
					break
				}
			}
		}

		if len(reasons) == 0 {
			log.Printf("Node %s PASSED predicate checks.", node.Name)

			// Taint checks
			if isNoPrefered {
				unPreferredNodes = append(unPreferredNodes, node)
			} else {
				compatibleNodes = append(compatibleNodes, node)
			}

		} else {
			log.Printf("Node %s FAILED predicate checks: %s", node.Name, strings.Join(reasons, "; "))
			fitFailures[node.Name] = reasons
		}
	}

	// 6. Handle case where no nodes are compatible
	if len(compatibleNodes) == 0 && len(unPreferredNodes) == 0 {
		log.Printf("Pod %s/%s failed to fit on any node.", podToSchedule.Namespace, podToSchedule.Name)
		failureMsg := fmt.Sprintf("pod (%s/%s) failed predicate checks on all nodes.", podToSchedule.Namespace, podToSchedule.Name)
		// Optionally add details:
		// for nodeName, reasons := range fitFailures {
		// 	failureMsg += fmt.Sprintf("\n Node %s: %s", nodeName, strings.Join(reasons, "; "))
		// }

		_ = postEvent(ctx, clientset, podToSchedule, "FailedScheduling", failureMsg, "Warning") // Ignore event posting error
		return nil, errors.New("no compatible nodes found after predicate checks")              // Return specific error
	} else if len(compatibleNodes) == 0 && len(unPreferredNodes) > 0 {
		log.Printf("Pod %s/%s failed to find a compatible node, but fit on unpreferred nodes.", podToSchedule.Namespace, podToSchedule.Name)
		return unPreferredNodes, nil // Return unpreferred nodes
	} else {
		log.Printf("Found %d compatible nodes for pod %s/%s.", len(compatibleNodes), podToSchedule.Namespace, podToSchedule.Name)
		return compatibleNodes, nil
	}
}

// bindPod assigns the pod to the chosen node by creating a Binding object.
func bindPod(ctx context.Context, clientset *kubernetes.Clientset, podToSchedule *v1.Pod, nodeName string) error {
	log.Printf("Attempting to bind pod %s/%s to node %s", podToSchedule.Namespace, podToSchedule.Name, nodeName)

	binding := &v1.Binding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podToSchedule.Name,
			Namespace: podToSchedule.Namespace,
			// UID: types.UID(SchedulerName + "-" + string(podToSchedule.UID)), // UID not needed for binding creation
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
			ResourceVersion: pod.ResourceVersion,
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
	}

	_, err := clientset.CoreV1().Events(pod.Namespace).Create(ctx, event, metav1.CreateOptions{})
	if err != nil {
		log.Printf("Warning: Failed to create event (Reason: %s, Pod: %s/%s): %v", reason, pod.Namespace, pod.Name, err)
	}
	return err // Return error, but caller might ignore it
}
