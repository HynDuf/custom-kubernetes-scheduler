# Custom Kubernetes Scheduler

## Table of Contents

1.  [Project Goal](#project-goal)
2.  [Features](#features)
3.  [How it Works](#how-it-works)
4.  [Tech Stack](#tech-stack)
5.  [Prerequisites](#prerequisites)
6.  [Setup and Deployment](#setup-and-deployment)
    1.  [1. Clone the Repository](#clone-the-repository)
    2.  [2. Configure GKE and Build/Push Scheduler Image](#configure-gke-and-build-and-push-scheduler-image)
    3.  [3. Deploy Monitoring Stack (Prometheus & Node Exporter)](#deploy-monitoring-stack)
    4.  [4. Access Prometheus UI (Optional)](#access-prometheus-ui)
    5.  [5. Deploy the Custom Scheduler](#deploy-the-custom-scheduler)
7.  [Configuration](#configuration)
    1.  [Scheduler Name](#scheduler-name)
    2.  [Scoring Weights](#scoring-weights)
8.  [Usage and Demonstration](#usage-and-demonstration)
    1.  [Demo Scenarios](#demo-scenarios)
        1.  [Demo 1: Resource-Aware Scheduling](#demo-1-resource-aware-scheduling)
        2.  [Demo 2: Node Selector](#demo-2-node-selector)
        3.  [Demo 3: Taints and Tolerations](#demo-3-taints-and-tolerations)
        4.  [Demo 4: Node Affinity](#demo-4-node-affinity)
9.  [Local Development (Alternative to GKE deployment)](#local-development-alternative-to-gke-deployment)
10. [Cleanup](#cleanup)
11. [Directory Structure](#directory-structure)
12. [References](#references)
## Project Goal

This project implements a custom Kubernetes scheduler designed to optimize pod placement based on a combination of real-time node metrics and pod scheduling preferences. Unlike the default Kubernetes scheduler, which primarily considers resource *requests*, this custom scheduler makes decisions based on:

1.  **Actual Available Memory:** Uses `node_memory_MemAvailable_bytes` from Prometheus.
2.  **Actual Available CPU:** Uses average idle CPU cores (`avg by (instance) (rate(node_cpu_seconds_total{mode="idle"}[1m]))`) from Prometheus.
3.  **Pod Node Affinity:** Respects `preferredDuringSchedulingIgnoredDuringExecution` node affinity rules defined in Pod specs.

For each unscheduled pod, the scheduler identifies compatible nodes (meeting the pod's CPU and memory requests and taints/tolerations) and then scores them based on a weighted combination of the above factors. The node with the highest score is selected.

## Features

*   **Real-time Metric-Based Scheduling:** Utilizes live memory and CPU data from Prometheus for more accurate load assessment.
*   **Configurable Scoring Weights:** Allows tuning the importance of available memory, available CPU, and node affinity through environment variables.
*   **Node Affinity Support:** Considers `preferredDuringSchedulingIgnoredDuringExecution` for node scoring, allowing pods to express preferences for certain nodes.
*   **Extensible Predicate Checks:** Performs standard Kubernetes predicate checks (resource requirements, taints/tolerations, node selectors).
*   **Event Reporting:** Creates Kubernetes events for scheduling decisions and failures.
*   **Built with Go and Client-Go:** Leverages the official Kubernetes Go client library.

## How it Works

The custom scheduler performs the following main tasks:

1.  **Watch for Unscheduled Pods:** Continuously monitors the Kubernetes API for newly created pods that are assigned to `custom-scheduler` (via `spec.schedulerName: custom-scheduler`) and are in a `Pending` state with no node assigned.
2.  **Filter Nodes (Predicates):** For each unscheduled pod, it identifies a list of "compatible" nodes. This involves:
    *   Checking if the node can satisfy the pod's resource requests (CPU and memory).
    *   Ensuring the pod's tolerations can accommodate the node's taints (excluding `PreferNoSchedule` taints, which are handled during scoring).
    *   Matching node selectors defined in the pod spec.
3.  **Score Nodes (Priorities):** Each compatible node is then scored based on:
    *   **Available Memory Score:** Normalized score based on `node_memory_MemAvailable_bytes`. Higher available memory gets a better score.
    *   **Available CPU Score:** Normalized score based on average idle CPU cores. More idle CPU capacity gets a better score.
    *   **Node Affinity Score:** Score bonus based on matching `preferredDuringSchedulingIgnoredDuringExecution` rules in the pod's affinity spec.
    *   **Total Score:** A weighted sum of the individual scores. Weights (`SCORE_WEIGHT_MEM`, `SCORE_WEIGHT_CPU`, `SCORE_WEIGHT_AFFINITY`) are configurable.
4.  **Select Best Node:** The compatible node with the highest total score is chosen.
5.  **Bind Pod to Node:** The scheduler assigns the pod to the selected node by creating a Kubernetes `Binding` object.

This approach aims to distribute pods more effectively based on actual resource availability and pod preferences, potentially leading to better cluster utilization and performance.

## Tech Stack

*   **Kubernetes:** For container orchestration.
*   **GoLang:** For the custom scheduler development, using `client-go`.
*   **Prometheus:** For collecting and exposing node metrics.
    *   **Prometheus Node Exporter:** To scrape node-level metrics.
*   **Docker:** For containerizing the custom scheduler.
*   **YAML:** For defining Kubernetes resources (Deployments, Services, RBAC, etc.).

## Prerequisites

*   A running Kubernetes cluster (e.g., GKE, Minikube, Kind, Kubeadm-dind-cluster).
*   `kubectl` configured to communicate with your cluster.
*   Docker (if building the scheduler image).
*   Go programming language (if modifying or building the scheduler locally).
*   Git for cloning the repository.

## Setup and Deployment

These instructions primarily focus on deploying to **Google Kubernetes Engine (GKE)** but can be adapted for other clusters.

### Clone the Repository

```sh
git clone https://github.com/HynDuf/custom-kubernetes-scheduler.git # Or your fork
cd custom-kubernetes-scheduler
```

### Configure GKE and Build and Push Scheduler Image

```sh
# For GKE Users
gcloud config set project [YOUR_PROJECT_ID]
gcloud container clusters get-credentials [YOUR_CLUSTER_NAME] --zone [YOUR_COMPUTE_ZONE]

# Build and Push the Docker image
cd scheduler
PROJECT_ID=$(gcloud config get-value project) # For GKE/GCR
# If not using GCR, replace gcr.io/${PROJECT_ID} with your Docker registry path
docker build -t gcr.io/${PROJECT_ID}/custom-scheduler:v1 .
gcloud auth configure-docker gcr.io --quiet # For GCR
docker push gcr.io/${PROJECT_ID}/custom-scheduler:v1
cd ..

# IMPORTANT: Edit the scheduler image name in scheduler/custom-scheduler-deployment.yaml
# Open scheduler/custom-scheduler-deployment.yaml and update the image field:
# image: gcr.io/[YOUR_PROJECT_ID]/custom-scheduler:v1 # Replace [YOUR_PROJECT_ID]
```
*After editing, save `scheduler/custom-scheduler-deployment.yaml`.*

### Deploy Monitoring Stack

```sh
# Create 'monitoring' namespace
kubectl create namespace monitoring

# Deploy Node Exporter (collects metrics from each node)
kubectl apply -f node-exporter/node-exporter-daemonset.yml

# Deploy Prometheus RBAC, ConfigMap, Deployment, and Service
kubectl apply -f prometheus/clusterRole.yaml
kubectl apply -f prometheus/config-map.yaml -n monitoring
kubectl apply -f prometheus/prometheus-deployment.yaml -n monitoring
kubectl apply -f prometheus/prometheus-service.yaml -n monitoring

# Verify monitoring components (wait for pods to be Running)
kubectl get pods -o wide -n monitoring
# You should see one node-exporter pod per node, and one prometheus-deployment pod.
```

### Access Prometheus UI

```sh
# Forward Prometheus service port to your local machine
kubectl port-forward svc/prometheus-service 9090:8080 -n monitoring
# Open your browser and go to http://localhost:9090
# You can test queries like:
# - node_memory_MemAvailable_bytes
# - avg by (instance) (rate(node_cpu_seconds_total{mode="idle"}[1m]))
```

### Deploy the Custom Scheduler

```sh
# Apply RBAC rules for the custom scheduler
kubectl apply -f scheduler/scheduler-rbac.yaml

# Deploy the custom scheduler
kubectl apply -f scheduler/custom-scheduler-deployment.yaml

# Watch scheduler logs
kubectl logs -n kube-system -l app=custom-scheduler -f --tail=100
```

## Configuration

### Scheduler Name

Pods intended to be scheduled by this custom scheduler must specify `custom-scheduler` in their `spec.schedulerName` field:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: my-pod
spec:
  schedulerName: custom-scheduler # This tells Kubernetes to use our scheduler
  containers:
  - name: my-container
    image: nginx
```

### Scoring Weights

The scoring behavior of the scheduler can be tuned using environment variables in the `scheduler/custom-scheduler-deployment.yaml` file:

*   `SCORE_WEIGHT_MEM`: Weight for available memory score (default: `0.6`, check `custom-scheduler-deployment.yaml`).
*   `SCORE_WEIGHT_CPU`: Weight for available CPU score (default: `0.4`, check `custom-scheduler-deployment.yaml`).
*   `SCORE_WEIGHT_AFFINITY`: Multiplier for the node affinity score (default: `1.0`, check `custom-scheduler-deployment.yaml`).

Modify these values in the deployment YAML and re-apply it to change the scheduler's behavior.

## Usage and Demonstration

### Demo Scenarios

Now, let's demonstrate the custom scheduler's capabilities.

#### Demo 1: Resource-Aware Scheduling

This demo highlights how the custom scheduler considers actual resource usage (via Prometheus metrics), unlike the default scheduler which primarily looks at resource *requests*.

1.  **Deploy baseline pods:**
    *   `sleep.yaml`: Requests 1600Mi Memory, but actually uses very little.
    *   `sysbench.yaml`: Requests 0Mi Memory, but actually consumes nearly 2GiB of Memory (after a short startup).
    ```sh
    kubectl apply -f scheduler/deployments/sleep.yaml
    kubectl apply -f scheduler/deployments/sysbench.yaml
    ```

2.  **Wait and observe:**
    Wait for these pods to be scheduled and running. Check their status and on which nodes they are placed:
    ```sh
    kubectl get pods -o wide
    ```
    Note the nodes where `sysbench-*` and `sleep-*` are running.

3.  **Examine node resource usage:**
    Replace `[NODE_NAME_RUNNING_SYSBENCH]` and `[NODE_NAME_RUNNING_SLEEP]` with the actual node names.
    ```sh
    # For the node running sysbench:
    kubectl describe node [NODE_NAME_RUNNING_SYSBENCH]
    # For the node running sleep:
    kubectl describe node [NODE_NAME_RUNNING_SLEEP]
    ```
    *   Observe in the `Allocated resources` section. The `sysbench` node will have low *requested* memory but Prometheus (if you check `node_memory_MemAvailable_bytes` for that node) will show high *actual* memory consumption.
    *   The `sleep` node will have high *requested* memory but Prometheus will show low *actual* memory consumption.

4.  **Deploy a test pod using the Default Scheduler:**
    The `testdefault.yaml` deployment does *not* specify a `schedulerName`.
    ```sh
    kubectl apply -f scheduler/deployments/testdefault.yaml
    ```
    Check pod placement. The default scheduler will likely place this pod based on resource *requests*, potentially choosing the node already stressed by `sysbench` if its *requested* resources seem low.
    ```sh
    kubectl get pods -o wide 
    ```

5.  **Deploy a test pod using the Custom Scheduler:**
    The `testcustom.yaml` deployment specifies `schedulerName: custom-scheduler`.
    ```sh
    kubectl apply -f scheduler/deployments/testcustom.yaml
    ```
    Check the custom scheduler logs. The pod should be placed on a node that the custom scheduler determined to have better actual resource availability (likely the node running the `sleep` pod or another less loaded node).
    ```sh
    kubectl get pods -o wide
    # Observe the custom scheduler logs 
    kubectl logs -n kube-system -l app=custom-scheduler -f --tail=100
    ```

6.  **Cleanup Demo 1:**
    ```sh
    kubectl delete -f scheduler/deployments/testdefault.yaml --ignore-not-found
    kubectl delete -f scheduler/deployments/testcustom.yaml --ignore-not-found
    # Optionally, delete sleep and sysbench if you want to free up resources for other demos,
    # or keep them to see how they influence other scheduling decisions.
    # kubectl delete -f scheduler/deployments/sleep.yaml --ignore-not-found
    # kubectl delete -f scheduler/deployments/sysbench.yaml --ignore-not-found
    ```

#### Demo 2: Node Selector

This demo shows how the custom scheduler respects `nodeSelector` constraints.

1.  **Label a node:**
    Replace `[CHOSEN_NODE_FOR_LABEL]` with an actual node name from `kubectl get nodes`.
    ```sh
    kubectl label node [CHOSEN_NODE_FOR_LABEL] group=red --overwrite
    ```

2.  **Deploy a pod with a `nodeSelector`:**
    The `scheduler/deployments/pod-node-selector-1.yaml` specifies `nodeSelector: { group: red }`.
    ```sh
    kubectl apply -f scheduler/deployments/pod-node-selector-1.yaml
    ```

3.  **Check pod placement and logs:**
    The pod should be scheduled on the node you labeled `group=red`. Observe the custom scheduler logs for filtering steps.
    ```sh
    kubectl get pods -o wide -l app=nginx-ns-1 # Assuming the pod has a label app=nginx-ns-1 or similar. Update if needed based on pod-node-selector-1.yaml
    # Observe the custom scheduler logs 
    kubectl logs -n kube-system -l app=custom-scheduler -f --tail=100
    ```

4.  **Cleanup Demo 2:**
    ```sh
    kubectl delete -f scheduler/deployments/pod-node-selector-1.yaml --ignore-not-found
    kubectl label node [CHOSEN_NODE_FOR_LABEL] group- # Remove the label
    ```

#### Demo 3: Taints and Tolerations

This demo shows how the custom scheduler handles taints on nodes and tolerations on pods.

1.  **Taint nodes:**
    Replace `[CHOSEN_NODE_1_FOR_TAINT]` and `[CHOSEN_NODE_2_FOR_TAINT]` with actual, distinct node names.
    ```sh
    kubectl taint nodes [CHOSEN_NODE_1_FOR_TAINT] key1=value1:NoSchedule --overwrite
    kubectl taint nodes [CHOSEN_NODE_2_FOR_TAINT] key2=value2:NoSchedule --overwrite
    ```

2.  **Deploy a pod with specific tolerations:**
    The `scheduler/deployments/pod-toleration-1.yaml` pod only tolerates `key1=value1:NoSchedule`.
    ```sh
    kubectl apply -f scheduler/deployments/pod-toleration-1.yaml
    ```

3.  **Check pod placement and logs:**
    The pod should be scheduled on `[CHOSEN_NODE_1_FOR_TAINT]` (if resources allow) or any other untainted, eligible node. It should *not* be scheduled on `[CHOSEN_NODE_2_FOR_TAINT]` because it doesn't tolerate `key2=value2`. Observe the custom scheduler logs.
    ```sh
    kubectl get pods -o wide -l app=nginx-taint-1 # Assuming the pod has a label app=nginx-taint-1 or similar. Update if needed.
    # Observe the custom scheduler logs 
    kubectl logs -n kube-system -l app=custom-scheduler -f --tail=100
    ```

4.  **Cleanup Demo 3:**
    ```sh
    kubectl delete -f scheduler/deployments/pod-toleration-1.yaml --ignore-not-found
    kubectl taint nodes [CHOSEN_NODE_1_FOR_TAINT] key1=value1:NoSchedule-
    kubectl taint nodes [CHOSEN_NODE_2_FOR_TAINT] key2=value2:NoSchedule-
    ```

#### Demo 4: Node Affinity

This demo illustrates `preferredDuringSchedulingIgnoredDuringExecution` node affinity.

1.  **Label nodes for affinity:**
    Replace `[CHOSEN_NODE_1_FOR_AFFINITY_LABEL]` and `[CHOSEN_NODE_2_FOR_AFFINITY_LABEL]` with actual, distinct node names.
    ```sh
    kubectl label node [CHOSEN_NODE_1_FOR_AFFINITY_LABEL] zone=a --overwrite
    kubectl label node [CHOSEN_NODE_2_FOR_AFFINITY_LABEL] zone=b --overwrite
    ```

2.  **Deploy a pod with preferred node affinity:**
    The `scheduler/deployments/pod-preferred-affinity.yaml` pod prefers nodes with `zone=a` (weight 80) and then `zone=b` (weight 20).
    ```yaml
    # Excerpt from scheduler/deployments/pod-preferred-affinity.yaml:
    # spec:
    #   schedulerName: custom-scheduler
    #   affinity:
    #     nodeAffinity:
    #       preferredDuringSchedulingIgnoredDuringExecution:
    #       - weight: 80
    #         preference:
    #           matchExpressions:
    #           - key: zone
    #             operator: In
    #             values: ["a"]
    #       - weight: 20
    #         preference:
    #           matchExpressions:
    #           - key: zone
    #             operator: In
    #             values: ["b"]
    ```
    ```sh
    kubectl apply -f scheduler/deployments/pod-preferred-affinity.yaml
    ```

3.  **Check pod placement and logs:**
    The pod should ideally be scheduled on the node labeled `zone=a` if compatible and available, or `zone=b` otherwise, considering other scoring factors. Check the custom scheduler logs to see how affinity scores influenced the decision.
    ```sh
    kubectl get pods -o wide -l app=nginx-preferred-affinity # Assuming the pod has a label app=nginx-preferred-affinity or similar. Update if needed.
    # Observe the custom scheduler logs 
    kubectl logs -n kube-system -l app=custom-scheduler -f --tail=100
    ```

4.  **Cleanup Demo 4:**
    ```sh
    kubectl delete -f scheduler/deployments/pod-preferred-affinity.yaml --ignore-not-found
    kubectl label node [CHOSEN_NODE_1_FOR_AFFINITY_LABEL] zone-
    kubectl label node [CHOSEN_NODE_2_FOR_AFFINITY_LABEL] zone-
    ```

---

**End of Demo**

Remember to clean up resources if you no longer need them (see "Cleanup" section below).
To stop the Prometheus port-forward, find its process ID (e.g., using `ps aux | grep "kubectl port-forward svc/prometheus-service"`) and use `kill [PID]`.

---

## Local Development (Alternative to GKE deployment)

If you want to run the scheduler locally for development (e.g., against a Minikube or Kind cluster):

1.  **Ensure `kubectl` is configured for your local cluster.**
2.  **Run `kubectl proxy`:**
    This allows the scheduler (running outside the cluster) to access the Kubernetes API.
    ```sh
    kubectl proxy &
    ```
    The API will typically be available at `http://localhost:8001`. Your scheduler code might need to be adjusted to use this endpoint if it's hardcoded for in-cluster config (the provided `kubernetes.go` attempts in-cluster config first).
3.  **Build and Run the Scheduler:**
    Navigate to the `scheduler/` directory.
    ```sh
    cd scheduler
    go build .
    ./scheduler
    # Or: go run .
    ```
    The scheduler will start, connect to the Kubernetes API via the proxy, and begin watching for pods.
4.  **Deploy test pods:**
    You can adapt the demo scenarios above for local testing. For example:
    *   Deploy `testcustom.yaml` and `testdefault.yaml` to see scheduling decisions.
    *   Label your Minikube/Kind node(s) and test `pod-node-selector-1.yaml`.
    *   Taint your node(s) and test `pod-toleration-1.yaml`.
    *   Label your node(s) and test `pod-preferred-affinity.yaml`.

*Note: For local development, ensure Prometheus is also accessible by the scheduler if it's running outside the cluster and you want to use its metrics. This might involve port-forwarding the Prometheus service or configuring an external endpoint in your scheduler's Prometheus client.*

## Cleanup

To remove all components deployed by this project:

```sh
# Delete sample deployments/pods (ensure these match files used in demos)
kubectl delete -f scheduler/deployments/pod-node-selector-1.yaml --ignore-not-found
# If you used pod-node-selector-2.yaml or 3.yaml from old tests, add them here
# kubectl delete -f scheduler/deployments/pod-node-selector-2.yaml --ignore-not-found
# kubectl delete -f scheduler/deployments/pod-node-selector-3.yaml --ignore-not-found

kubectl delete -f scheduler/deployments/pod-toleration-1.yaml --ignore-not-found
# If you used pod-toleration-2.yaml or 3.yaml from old tests, add them here
# kubectl delete -f scheduler/deployments/pod-toleration-2.yaml --ignore-not-found
# kubectl delete -f scheduler/deployments/pod-toleration-3.yaml --ignore-not-found

kubectl delete -f scheduler/deployments/pod-preferred-affinity.yaml --ignore-not-found

kubectl delete -f scheduler/deployments/testcustom.yaml --ignore-not-found
kubectl delete -f scheduler/deployments/testdefault.yaml --ignore-not-found
kubectl delete -f scheduler/deployments/sysbench.yaml --ignore-not-found
kubectl delete -f scheduler/deployments/sleep.yaml --ignore-not-found

# Delete custom scheduler components
kubectl delete -f scheduler/custom-scheduler-deployment.yaml --ignore-not-found
kubectl delete -f scheduler/scheduler-rbac.yaml --ignore-not-found

# Delete monitoring stack components
kubectl delete -f prometheus/prometheus-service.yaml -n monitoring --ignore-not-found
kubectl delete -f prometheus/prometheus-deployment.yaml -n monitoring --ignore-not-found
kubectl delete -f prometheus/config-map.yaml -n monitoring --ignore-not-found
kubectl delete -f prometheus/clusterRole.yaml --ignore-not-found # Deletes ClusterRole and associated ClusterRoleBinding
kubectl delete -f node-exporter/node-exporter-daemonset.yml --ignore-not-found
kubectl delete namespace monitoring --ignore-not-found

# For GKE/GCR users: Delete the Docker image
PROJECT_ID=$(gcloud config get-value project)
gcloud container images delete "gcr.io/${PROJECT_ID}/custom-scheduler:v1" --force-delete-tags --quiet

# For GKE users: Delete the cluster (optional)
# echo "To delete your GKE cluster, run: gcloud container clusters delete [YOUR_CLUSTER_NAME] --zone [YOUR_COMPUTE_ZONE] --quiet"
```

## Directory Structure

```
.
├── LICENSE
├── node-exporter
│   └── node-exporter-daemonset.yml
├── prometheus
│   ├── clusterRole.yaml
│   ├── config-map.yaml
│   ├── example.yaml
│   ├── prometheus-deployment.yaml
│   └── prometheus-service.yaml
├── DEMO.md
├── README.md                               --> This file
└── scheduler
    ├── custom-scheduler
    ├── custom-scheduler-deployment.yaml
    ├── deployments
    │   ├── pod-node-selector-1.yaml
    │   ├── pod-node-selector-2.yaml
    │   ├── pod-node-selector-3.yaml
    │   ├── pod-preferred-affinity.yaml
    │   ├── pod-toleration-1.yaml
    │   ├── pod-toleration-2.yaml
    │   ├── pod-toleration-3.yaml
    │   ├── sleep.yaml
    │   ├── sysbench.yaml
    │   ├── testcustom.yaml
    │   └── testdefault.yaml
    ├── Dockerfile
    ├── getBestNode.go
    ├── go.mod
    ├── go.sum
    ├── kubernetes.go
    ├── main.go
    ├── node_memory_MemTotal.json
    ├── processor.go
    ├── scheduler-rbac.yaml
    ├── scoring.go
    └── types.go

5 directories, 33 files
```

## References

*   [GopherCon 2016: Kelsey Hightower - Building a custom Kubernetes scheduler](https://www.youtube.com/watch?v=IYcL0Un1io0)
*   [Hightower Toy Scheduler Source Code](https://github.com/kelseyhightower/scheduler) (Base inspiration for earlier versions of this project)
*   [Kubernetes Documentation: Scheduler](https://kubernetes.io/docs/concepts/scheduling-eviction/kube-scheduler/)
*   [Prometheus Documentation](https://prometheus.io/docs/introduction/overview/)

