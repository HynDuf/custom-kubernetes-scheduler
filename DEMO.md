```sh
# --- Prerequisites ---
# 1. A Google Kubernetes Engine (GKE) cluster is created.
# 2. `gcloud` CLI is installed and configured.
# 3. `kubectl` is installed and configured to communicate with your GKE cluster.
# 4. Docker is installed and running.

# --- Configure GKE and Clone Repository ---
# Configure gcloud to use your project and get cluster credentials
gcloud config set project [YOUR_PROJECT_ID]
gcloud container clusters get-credentials [YOUR_CLUSTER_NAME] --zone [YOUR_COMPUTE_ZONE]

# Clone the custom scheduler repository (or your fork)
git clone https://github.com/HynDuf/custom-kubernetes-scheduler.git
cd custom-kubernetes-scheduler

# --- Build and Push Custom Scheduler Docker Image ---
cd scheduler
# The following commands build and push the scheduler image to Google Container Registry (GCR).
# Ensure PROJECT_ID is correctly captured from your gcloud config.
PROJECT_ID=$(gcloud config get-value project)
docker build -t "gcr.io/${PROJECT_ID}/custom-scheduler:v1" .
gcloud auth configure-docker gcr.io --quiet # Authenticate Docker with GCR
docker push "gcr.io/${PROJECT_ID}/custom-scheduler:v1"
cd ..

# --- IMPORTANT: Update Scheduler Deployment Manifest ---
# You MUST manually edit the scheduler's deployment manifest to use the image you just pushed.
# Open the file: scheduler/custom-scheduler-deployment.yaml
# Find the `image` field under the container spec and update it to:
#   image: gcr.io/[YOUR_PROJECT_ID]/custom-scheduler:v1  (Replace [YOUR_PROJECT_ID] with your actual Project ID)

# --- Deploy Monitoring Components (Prometheus & Node Exporter) ---
# Create a dedicated namespace for monitoring components
kubectl create namespace monitoring

# Deploy Node Exporter (collects hardware and OS metrics from each node)
kubectl apply -f node-exporter/node-exporter-daemonset.yml

# Deploy Prometheus (monitoring system and time series database)
# This includes RBAC, ConfigMap, Deployment, and Service for Prometheus
kubectl apply -f prometheus/clusterRole.yaml
kubectl apply -f prometheus/config-map.yaml -n monitoring
kubectl apply -f prometheus/prometheus-deployment.yaml -n monitoring
kubectl apply -f prometheus/prometheus-service.yaml -n monitoring

# Verify monitoring components are running
kubectl get pods -o wide -n monitoring
# You should see one `node-exporter-*` pod per node 
# and one `prometheus-deployment-*` pod in the `monitoring` namespace.

# --- Access Prometheus UI ---
# Forward the Prometheus service port to your local machine
# (This command will run in the background. Note its PID if you want to kill it later.)
kubectl port-forward svc/prometheus-service 9090:8080 -n monitoring &

# Access the Prometheus UI in your browser at: http://localhost:9090
# You can test queries like:
# - node_memory_MemAvailable_bytes
# - avg by (instance) (rate(node_cpu_seconds_total{mode="idle"}[1m]))

# --- Deploy the Custom Scheduler ---
# Apply RBAC rules for the custom scheduler
kubectl apply -f scheduler/scheduler-rbac.yaml
# Deploy the custom scheduler itself (ensure you've updated the image name in this YAML)
kubectl apply -f scheduler/custom-scheduler-deployment.yaml

# --- Watch Custom Scheduler Logs ---
kubectl logs -n kube-system -l app=custom-scheduler -f --tail=100

# --- Demo: Resource-Aware Scheduling ---
# This demo highlights how the custom scheduler considers actual resource usage,
# unlike the default scheduler which primarily looks at resource requests.

# Deploy 'sleep.yaml': Requests 1600Mi Memory, but actually uses very little.
# Deploy 'sysbench.yaml': Requests 0Mi Memory, but actually consumes nearly 2GiB of Memory.
kubectl apply -f scheduler/deployments/sleep.yaml
kubectl apply -f scheduler/deployments/sysbench.yaml

# Wait for these pods to be scheduled and running.
# Check their status and on which nodes they are placed:
kubectl get pods -o wide

# Observe node resource usage.
# Then, describe the node where 'sysbench-*' pod and the node where 'sleep-*' is running:
# You'll notice SYSBENCH node has low *requested* memory but high *actual* memory consumption due to sysbench.
kubectl describe node [NODE_NAME_RUNNING_SYSBENCH]

# You'll notice SLEEP node has high *requested* memory but low *actual* memory consumption.
kubectl describe node [NODE_NAME_RUNNING_SLEEP]

# Deploy 'testdefault.yaml' using the default Kubernetes scheduler.
# The default scheduler will place this pod on the node already stressed by 'sysbench.yaml'
# because it doesn't considers actual memory usage.
kubectl apply -f scheduler/deployments/testdefault.yaml
# Check pod placement:
kubectl get pods -o wide

# Deploy 'testcustom.yaml' which will be scheduled by our custom scheduler.
# The custom scheduler should ideally place this pod on a less loaded node.
kubectl apply -f scheduler/deployments/testcustom.yaml
# Check pod placement and observe custom scheduler logs:
kubectl get pods -o wide
kubectl logs -n kube-system -l app=custom-scheduler -f --tail=100

# Clean up
kubectl delete -f scheduler/deployments/testdefault.yaml
kubectl delete -f scheduler/deployments/testcustom.yaml

# --- Demo: Node Selector ---
# Label a node (replace [CHOSEN_NODE_FOR_LABEL] with an actual node name from `kubectl get nodes`):
kubectl label node [CHOSEN_NODE_FOR_LABEL] group=red --overwrite

# Deploy a pod that uses a nodeSelector to target the labeled node:
kubectl apply -f scheduler/deployments/pod-node-selector-1.yaml
# Check pod placement and custom scheduler logs:
kubectl get pods -o wide -l app=pod-node-selector-1
kubectl logs -n kube-system -l app=custom-scheduler -f --tail=100

# Clean up
kubectl delete -f scheduler/deployments/pod-node-selector-1.yaml

# --- Demo: Taints and Tolerations ---
# Taint two different nodes (replace placeholders with actual node names from `kubectl get nodes`):
kubectl taint nodes [CHOSEN_NODE_1_FOR_TAINT] key1=value1:NoSchedule --overwrite
kubectl taint nodes [CHOSEN_NODE_2_FOR_TAINT] key2=value2:NoSchedule --overwrite

# Deploy 'pod-toleration-1.yaml'. This pod only tolerates 'key1=value1:NoSchedule'.
# It should be scheduled on [CHOSEN_NODE_1_FOR_TAINT] or the remaining node.
# [CHOSEN_NODE_2_FOR_TAINT] will not pass the predicate check because the pod doesn't tolerate its taint.
kubectl apply -f scheduler/deployments/pod-toleration-1.yaml
# Check pod placement and custom scheduler logs:
kubectl get pods -o wide -l app=pod-toleration-1
kubectl logs -n kube-system -l app=custom-scheduler -f --tail=100

# Clean up taints
kubectl taint nodes [CHOSEN_NODE_1_FOR_TAINT] key1:NoSchedule-
kubectl taint nodes [CHOSEN_NODE_2_FOR_TAINT] key2:NoSchedule-
kubectl delete -f scheduler/deployments/pod-toleration-1.yaml

# --- Demo: Node Affinity ---
# Label two different nodes (replace placeholders with actual node names from `kubectl get nodes`):
kubectl label node [CHOSEN_NODE_1_FOR_AFFINITY_LABEL] zone=a
kubectl label node [CHOSEN_NODE_2_FOR_AFFINITY_LABEL] zone=b

# Deploy 'pod-preferred-affinity.yaml'. This pod has a preferredNodeSchedulingIgnoredDuringExecution
# affinity for nodes with label 'zone=a' (weight: 80) or 'zone=b' (weight: 20).
kubectl apply -f scheduler/deployments/pod-preferred-affinity.yaml
# Check pod placement and custom scheduler logs:
kubectl get pods -o wide -l app=pod-preferred-affinity
kubectl logs -n kube-system -l app=custom-scheduler -f --tail=100

# Clean up labels
kubectl label node [CHOSEN_NODE_1_FOR_AFFINITY_LABEL] zone-
kubectl label node [CHOSEN_NODE_2_FOR_AFFINITY_LABEL] zone-

# --- End of Demo ---
# Remember to clean up resources if you no longer need them (e.g., delete deployments, services, namespaces, and the GKE cluster).
# To stop the Prometheus port-forward, find its process ID (e.g., using `ps aux | grep kubectl`) and use `kill [PID]`.
```
