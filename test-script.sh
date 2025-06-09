#!/bin/bash

# install kuberay operator
helm repo add kuberay https://ray-project.github.io/kuberay-helm/
helm repo update
helm install kuberay-operator kuberay/kuberay-operator --version 1.3.0

# deploy the controller
kubectl apply -f configmap.yaml
kubectl apply -f dscp-deployment.yaml

# deploy ray cluster in dscp-normal namespace
kubectl apply -f ray/ray-cluster-no-dscp.yaml

# wait for all worker pods to be ready (installing pytorch takes a while)

echo "Waiting for all worker pods to be ready"
sleep 30    
kubectl wait --for=condition=ready pod -l ray.io/cluster=raycluster-kuberay -n dscp-normal --timeout=900s || { echo "Failed waiting for worker pods to be ready"; exit 1; }


# generate traffic
echo "Generating traffic"
kubectl apply -f iperf3-1g-traffic-generator.yaml
sleep 5

echo "Waiting for iperf3 client to be ready"
kubectl wait --for=condition=ready pod -l app=iperf3-client -n bg-traffic --timeout=300s || { echo "Failed waiting for iperf3 client to be ready"; exit 1; }

echo "Submitting ray job"
# Get the Ray head service name
RAY_HEAD_SVC=$(kubectl get svc -n dscp-normal -l ray.io/node-type=head -o jsonpath='{.items[0].metadata.name}')
echo "Ray head service: $RAY_HEAD_SVC"

# Forward the Ray head service to localhost
kubectl port-forward svc/$RAY_HEAD_SVC 8265:8265 -n dscp-normal &>/dev/null &

# Wait for the port forward to be established
sleep 5

# Submit the Ray job
echo "Submitting ray job in dscp-normal namespace"
ray job submit --address=http://localhost:8265 --working-dir ray -- python3 ray_train_pytorch_mnist.py

# clean up
# Kill the port-forwarding process
pkill -f "kubectl port-forward"

kubectl delete -f iperf3-1g-traffic-generator.yaml
kubectl delete -f ray/ray-cluster-no-dscp.yaml

# deploy ray cluster in dscp-priority namespace
kubectl apply -f ray/ray-cluster-dscp-priority.yaml
sleep 5

# wait for ray cluster pods to be created first
echo "Waiting for Ray cluster pods to be created..."
sleep 30
kubectl wait --for=condition=ready pod -l ray.io/cluster=raycluster-kuberay -n dscp-priority --timeout=900s || { echo "Failed waiting for worker pods to be ready"; exit 1; }

# generate traffic
echo "Generating traffic"
kubectl apply -f iperf3-1g-traffic-generator.yaml
sleep 5

echo "Waiting for iperf3 client to be ready"
kubectl wait --for=condition=ready pod -l app=iperf3-client -n bg-traffic --timeout=300s || { echo "Failed waiting for iperf3 client to be ready"; exit 1; }

echo "Submitting ray job"
# Get the Ray head service name
RAY_HEAD_SVC=$(kubectl get svc -n dscp-priority -l ray.io/node-type=head -o jsonpath='{.items[0].metadata.name}')
echo "Ray head service: $RAY_HEAD_SVC"

# Forward the Ray head service to localhost
kubectl port-forward svc/$RAY_HEAD_SVC 8265:8265 -n dscp-priority &>/dev/null &

# Wait for the port forward to be established
sleep 5

# Submit the Ray job
echo "Submitting ray job in dscp-priority namespace"
ray job submit --address=http://localhost:8265 --working-dir ray -- python3 ray_train_pytorch_mnist.py


# clean up
# Kill the port-forwarding process
pkill -f "kubectl port-forward"

kubectl delete -f iperf3-1g-traffic-generator.yaml
kubectl delete -f ray/ray-cluster-dscp-priority.yaml


