# Custom-Kubernetes-Scheduler

## Run on GKE
```sh
gcloud config set project [YOUR_PROJECT_ID]
gcloud container clusters get-credentials $CLUSTER_NAME --zone $COMPUTE_ZONE

git clone https://github.com/HynDuf/custom-kubernetes-scheduler.git
cd scheduler
PROJECT_ID=$(gcloud config get-value project)
docker build -t gcr.io/${PROJECT_ID}/custom-scheduler:v1 .
gcloud auth configure-docker gcr.io --quiet
docker push gcr.io/${PROJECT_ID}/custom-scheduler:v1
cd ..
# Edit the image name in scheduler/custom-scheduler-deployment.yaml
# image: gcr.io/[YOUR_PROJECT_ID]/custom-scheduler:v1 # Replace [YOUR_PROJECT_ID]


kubectl create namespace monitoring
kubectl apply -f node-exporter/node-exporter-daemonset.yml
kubectl apply -f prometheus/clusterRole.yaml
kubectl apply -f prometheus/config-map.yaml -n monitoring
kubectl apply -f prometheus/prometheus-deployment.yaml -n monitoring
kubectl apply -f prometheus/prometheus-service.yaml -n monitoring

# See Prometheus UI
kubectl port-forward svc/prometheus-service 9090:8080 -n monitoring

# Check if there are 3 node exporters and 1 prometheus all RUNNING
kubectl get pods -o wide -n monitoring

kubectl apply -f scheduler/scheduler-rbac.yaml
kubectl apply -f scheduler/custom-scheduler-deployment.yaml

# Watch scheduler logs
kubectl logs -n kube-system -l app=custom-scheduler -f --tail=1000

kubectl apply -f scheduler/deployments/sleep.yaml
kubectl apply -f scheduler/deployments/sysbench.yaml

kubectl apply -f scheduler/deployments/testdefault.yaml
kubectl apply -f scheduler/deployments/testcustom.yaml
kubectl get pods -o wide

# Test for node affinity
kubectl label node {NODE_NAME} zone=a
kubectl label node {NODE_NAME1} zone=b
kubectl apply -f scheduler/deployments/pod-preferred-affinity.yaml

# DELETE
kubectl delete -f scheduler/deployments/pod-preferred-affinity.yaml

kubectl delete -f scheduler/deployments/testcustom.yaml
kubectl delete -f scheduler/deployments/testdefault.yaml
kubectl delete -f scheduler/deployments/sysbench.yaml
kubectl delete -f scheduler/deployments/sleep.yaml

kubectl delete -f scheduler/custom-scheduler-deployment.yaml
kubectl delete -f scheduler/scheduler-rbac.yaml

kubectl delete -f prometheus/prometheus-service.yaml -n monitoring
kubectl delete -f prometheus/prometheus-deployment.yaml -n monitoring
kubectl delete -f prometheus/config-map.yaml -n monitoring
kubectl delete -f prometheus/clusterRole.yaml # Deletes CRB too
kubectl delete -f node-exporter/node-exporter-daemonset.yml
kubectl delete namespace monitoring

PROJECT_ID=$(gcloud config get-value project)
gcloud container images delete gcr.io/${PROJECT_ID}/custom-scheduler:v1 --force-delete-tags --quiet

gcloud container clusters delete $CLUSTER_NAME --zone $COMPUTE_ZONE --quiet
```
