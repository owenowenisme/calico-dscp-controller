#!/bin/bash

docker build -t localhost:5000/dscp-controller .
docker push localhost:5000/dscp-controller
