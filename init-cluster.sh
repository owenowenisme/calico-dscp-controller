#!/bin/bash
sudo kubeadm init --pod-network-cidr=192.168.0.0/16
sudo cp -i /etc/kubernetes/admin.conf $HOME/.kube/config
kubectl taint nodes --all node-role.kubernetes.io/master:NoSchedule-
kubectl create -f https://raw.githubusercontent.com/projectcalico/calico/v3.29.3/manifests/tigera-operator.yaml
kubectl create -f https://raw.githubusercontent.com/projectcalico/calico/v3.29.3/manifests/custom-resources.yaml
kubectl apply -f rbac.yaml
kubectl apply -f configmap.yaml
export KUBECONFIG=$HOME/.kube/config