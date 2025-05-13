```sh
kubectl apply -f scheduler/custom-scheduler-deployment.yaml
kubectl logs -n kube-system -l app=custom-scheduler -f --tail=100

kubectl describe node gke-cluster-2-default-pool-c5dfe2a2-5sif
kubectl describe node gke-cluster-2-default-pool-c5dfe2a2-y0v6

kubectl apply -f scheduler/deployments/testdefault.yaml
kubectl get pods -o wide

kubectl apply -f scheduler/deployments/testcustom.yaml
kubectl get pods -o wide
kubectl logs -n kube-system -l app=custom-scheduler -f --tail=100

kubectl label node gke-cluster-2-default-pool-c5dfe2a2-y0v6 group=red --overwrite
kubectl apply -f scheduler/deployments/pod-node-selector-1.yaml
kubectl get pods -o wide
kubectl logs -n kube-system -l app=custom-scheduler -f --tail=100
kubectl label node gke-cluster-2-default-pool-c5dfe2a2-y0v6 group-
kubectl delete -f scheduler/deployments/pod-node-selector-1.yaml

kubectl taint nodes gke-cluster-2-default-pool-c5dfe2a2-5sif key1=value1:NoSchedule --overwrite
kubectl taint nodes gke-cluster-2-default-pool-c5dfe2a2-kci2 key2=value2:NoSchedule --overwrite
kubectl apply -f scheduler/deployments/pod-toleration-1.yaml
kubectl get pods -o wide
kubectl logs -n kube-system -l app=custom-scheduler -f --tail=100

kubectl taint nodes gke-cluster-2-default-pool-c5dfe2a2-5sif key1:NoSchedule-
kubectl taint nodes gke-cluster-2-default-pool-c5dfe2a2-kci2 key2:NoSchedule-
kubectl delete -f scheduler/deployments/pod-toleration-1.yaml

kubectl label node gke-cluster-2-default-pool-c5dfe2a2-kci2 zone=a
kubectl label node gke-cluster-2-default-pool-c5dfe2a2-y0v6 zone=b
kubectl apply -f scheduler/deployments/pod-preferred-affinity.yaml
kubectl get pods -o wide
kubectl logs -n kube-system -l app=custom-scheduler -f --tail=100

kubectl label node gke-cluster-2-default-pool-c5dfe2a2-kci2 zone-
kubectl label node gke-cluster-2-default-pool-c5dfe2a2-y0v6 zone-
kubectl delete -f scheduler/deployments/pod-preferred-affinity.yaml

kubectl delete -f scheduler/deployments/testcustom.yaml --ignore-not-found
kubectl delete -f scheduler/deployments/testdefault.yaml --ignore-not-found
kubectl delete -f scheduler/custom-scheduler-deployment.yaml
```
