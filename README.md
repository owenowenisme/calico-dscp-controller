# Calico-DSCP-Controller
A kubernetes controller that support traffic prioritization with DSCP marking.
Support both Calico or other iptables based CNI.

## Prerequisites

Before you try out this controller, you need to prepare:
- A kubernetes cluster with calico installed
- Nodes are connected by a network switch that supports DSCP marking
- The network switch need to be configured about DSCP marking rule , for example:
  - dscp-to-tc mapping
  - tc-to-queue mapping
  - scheduler strategy
  And bind the DSCP marking rule to the network interface, configuration might vary from different network switch vendors.

## How to install

1. Install the controller

```
kubectl apply -f https://raw.githubusercontent.com/owenowenisme/calico-dscp-controller/refs/heads/main/dscp-deployment.yaml
```

2. Deploy the configmap 
```
kubectl apply -f https://raw.githubusercontent.com/owenowenisme/calico-dscp-controller/refs/heads/main/configmap.yaml
```
You can modified the dscp priority of namespace by changing the value in configmap.

## System Architecture

![System Architecture](https://github.com/user-attachments/assets/a46c1f5e-1c5c-4d1c-a58e-00409c16df22)

![SysSwitch Queue Mapping](https://github.com/user-attachments/assets/03b241eb-bf47-459f-9370-8fc2b3acf144)
