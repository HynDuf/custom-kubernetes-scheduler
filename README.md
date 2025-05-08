# Custom Kubernetes Scheduler

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

### 1. Clone the Repository

```sh
git clone https://github.com/HynDuf/custom-kubernetes-scheduler.git # Or your fork
cd custom-kubernetes-scheduler
```

### 2. Configure GKE (if applicable) and Build/Push Scheduler Image

```sh
# For GKE Users
gcloud config set project [YOUR_PROJECT_ID]
# Example: gcloud container clusters create custom-sched-cluster --zone us-central1-c --num-nodes=3
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

### 3. Deploy Monitoring Stack (Prometheus & Node Exporter)

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

### 4. Access Prometheus UI (Optional)

```sh
# Forward Prometheus service port to your local machine
kubectl port-forward svc/prometheus-service 9090:8080 -n monitoring
# Open your browser and go to http://localhost:9090
# You can test queries like:
# - node_memory_MemAvailable_bytes
# - avg by (instance) (rate(node_cpu_seconds_total{mode="idle"}[1m]))
```

### 5. Deploy the Custom Scheduler

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

*   `SCORE_WEIGHT_MEM`: Weight for available memory score (default: `0.5` in old README, check `custom-scheduler-deployment.yaml` for current defaults).
*   `SCORE_WEIGHT_CPU`: Weight for available CPU score (default: `0.4` in `custom-scheduler-deployment.yaml`).
*   `SCORE_WEIGHT_AFFINITY`: Multiplier for the node affinity score (default: `1.0` in `custom-scheduler-deployment.yaml`).

Modify these values in the deployment YAML and re-apply it to change the scheduler's behavior.

## Usage and Testing

Ensure the custom scheduler pod is running in the `kube-system` namespace and you are tailing its logs.

### Example Scenario (based on old README test setup)

1.  **Deploy resource-intensive pods to establish a baseline:**
    The `sleep.yaml` deployment requests significant memory, and `sysbench.yaml` can run a memory-intensive workload.

    ```sh
    kubectl apply -f scheduler/deployments/sleep.yaml
    kubectl apply -f scheduler/deployments/sysbench.yaml

    # Wait for these pods to be scheduled and running
    kubectl get pods -o wide
    ```
    Observe which nodes these initial pods land on.

2.  **Deploy a test pod using the Default Scheduler:**
    The `testdefault.yaml` deployment does *not* specify a `schedulerName`, so it will be handled by the Kubernetes default scheduler.

    ```sh
    kubectl apply -f scheduler/deployments/testdefault.yaml
    kubectl get pods -o wide
    ```
    Observe where the default scheduler places this Nginx pod. It will likely choose a node based on resource *requests*.

3.  **Deploy a test pod using the Custom Scheduler:**
    The `testcustom.yaml` deployment specifies `schedulerName: custom-scheduler`.

    ```sh
    kubectl apply -f scheduler/deployments/testcustom.yaml
    kubectl get pods -o wide
    ```
    Check the custom scheduler logs. You should see entries related to filtering and scoring nodes for this Nginx pod. The pod should be placed on the node that the custom scheduler determined to have the best score based on available memory, CPU, and affinity (if any).

### Testing Node Selector
1. **Label nodes:**
   Assign group for each node to test node selector. For instance:
   ```sh
   kubectl label node {NODE_NAME_1} group=red
   kubectl label node {NODE_NAME_2} group=white
   kubectl label node {NODE_NAME_3} group=royal-blue
   ```
2. **Deploy pods with different group requiremts**
   Three files `scheduler/deployments/pod-node-selector-1.yaml`, `scheduler/deployments/pod-node-selector-2.yaml`, `scheduler/deployments/pod-node-selector-3.yaml` specify pods with group requirements
   ```yaml
   apiVersion: apps/v1
   kind: Deployment
   metadata:
     name: pod-node-selector-1
     labels:
       app: nginx-ns-3
   spec:
     replicas: 1
     selector:
       matchLabels:
         app: nginx-ns-3
     template:
       metadata:
         labels:
           app: nginx-ns-3
       spec:
         schedulerName: custom-scheduler
         nodeSelector:
           group: red
         containers:
           - name: nginx
             image: nginx:stable-alpine
             ports:
               - containerPort: 80
                 name: http
   ```
Deploy these pods:
    ```sh
    kubectl apply -f scheduler/deployments/pod-node-selector-1.yaml
    kubectl apply -f scheduler/deployments/pod-node-selector-2.yaml
    kubectl apply -f scheduler/deployments/pod-node-selector-3.yaml
    kubectl get pods -o wide
    ```
### Testing Taints/Tolerations
1. Add tains for nodes

```sh
kubectl taint nodes {NODE_NAME_1} key1=value1:NoSchedule
kubectl taint nodes {NODE_NAME_2} key2=value2:NoSchedule
kubectl taint nodes {NODE_NAME_3} key3=value3:NoSchedule
```

2. Deploy three deployments with suitable tolerations
Three files `scheduler/deployments/pod-toleration-1.yaml`, `scheduler/deployments/pod-toleration-2.yaml`, `scheduler/deployments/pod-toleration-3.yaml` specify pods
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: pod-toleration-1
  labels:
    app: nginx-taint-1
spec:
  replicas: 1
  selector:
    matchLabels:
      app: nginx-taint-1
  template:
    metadata:
      labels:
        app: nginx-taint-1
    spec:
      schedulerName: custom-scheduler
      tolerations:
        - key: "key1"
          operator: "Equal"
          value: "value1"
          effect: "NoSchedule"
      containers:
        - name: nginx
          image: nginx:stable-alpine
          ports:
            - containerPort: 80
              name: http
```
Deploy these pods:
```sh
kubectl apply -f scheduler/deployments/pod-toleration-1.yaml
kubectl apply -f scheduler/deployments/pod-toleration-2.yaml
kubectl apply -f scheduler/deployments/pod-toleration-3.yaml
kubectl get pods -o wide
```
### Testing Node Affinity

1.  **Label your nodes:**
    Assign labels to your nodes to test affinity rules. For example, to label nodes for different zones:

    ```sh
    # Find your node names
    kubectl get nodes

    # Label nodes (replace {NODE_NAME_1}, {NODE_NAME_2} with actual node names)
    kubectl label node {NODE_NAME_1} zone=a --overwrite
    kubectl label node {NODE_NAME_2} zone=b --overwrite
    # Add more labels as needed
    ```
3.  **Deploy a pod with preferred node affinity:**
    The `scheduler/deployments/pod-preferred-affinity.yaml` defines a pod that prefers nodes with `zone=a` (higher weight) and `zone=b` (lower weight).

    ```yaml
    # pod-preferred-affinity.yaml excerpt
    spec:
      schedulerName: custom-scheduler
      affinity:
        nodeAffinity:
          preferredDuringSchedulingIgnoredDuringExecution:
          - weight: 80 # High preference
            preference:
              matchExpressions:
              - key: zone
                operator: In
                values:
                - a
          - weight: 20 # Lower preference
            preference:
              matchExpressions:
              - key: zone
                operator: In
                values:
                - b
    ```

    Deploy this pod:
    ```sh
    kubectl apply -f scheduler/deployments/pod-preferred-affinity.yaml
    kubectl get pods -o wide
    ```
    Check the custom scheduler logs. You should see the affinity scores influencing the decision. The pod should ideally be scheduled on a node in `zone=a` if compatible and available, or `zone=b` otherwise, considering other scoring factors.

## Local Development (Alternative to full GKE deployment)

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
4.  **Deploy test pods as described in "Usage and Testing".**

*Note: For local development, ensure Prometheus is also accessible by the scheduler if it's running outside the cluster. This might involve port-forwarding the Prometheus service or configuring an external endpoint.*

## Cleanup

To remove all components deployed by this project:

```sh
# Delete sample deployments/pods
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
gcloud container images delete gcr.io/${PROJECT_ID}/custom-scheduler:v1 --force-delete-tags --quiet

# For GKE users: Delete the cluster (optional)
# gcloud container clusters delete [YOUR_CLUSTER_NAME] --zone [YOUR_COMPUTE_ZONE] --quiet
```

## Directory Structure

```
./
├── node-exporter
│   └── node-exporter-daemonset.yml
├── prometheus
│   ├── clusterRole.yaml
│   ├── config-map.yaml
│   ├── example.yaml
│   ├── prometheus-deployment.yaml
│   └── prometheus-service.yaml
├── scheduler
│   ├── deployments
│   │   ├── pod-preferred-affinity.yaml
│   │   ├── sleep.yaml
│   │   ├── sysbench.yaml
│   │   ├── testcustom.yaml
│   │   └── testdefault.yaml
│   ├── custom-scheduler-deployment.yaml
│   ├── Dockerfile
│   ├── getBestNode.go
│   ├── kubernetes.go
│   ├── main.go
│   ├── node_memory_MemTotal.json # Example Prometheus output
│   ├── processor.go
│   ├── scheduler-rbac.yaml
│   ├── scoring.go
│   └── types.go                  # Original types from Hightower example (project now uses client-go types)
└── README.md                     # This file
```

## References

*   [GopherCon 2016: Kelsey Hightower - Building a custom Kubernetes scheduler](https://www.youtube.com/watch?v=IYcL0Un1io0)
*   [Hightower Toy Scheduler Source Code](https://github.com/kelseyhightower/scheduler) (Base inspiration for earlier versions of this project)
*   [Kubernetes Documentation: Scheduler](https://kubernetes.io/docs/concepts/scheduling-eviction/kube-scheduler/)
*   [Prometheus Documentation](https://prometheus.io/docs/introduction/overview/)

