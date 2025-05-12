#!/bin/bash

docker build -t  $IMAGE_REGISTRY/dscp-controller .
docker push  $IMAGE_REGISTRY/dscp-controller
