# k8s-nautobot-node-label-controller

A Kubernetes controller that synchronizes node labels with Nautobot device data.

## Overview

This controller enables bidirectional synchronization between Kubernetes node labels and Nautobot device metadata. It helps maintain consistency between your Kubernetes infrastructure and your network source of truth.

## Features

- Synchronizes Kubernetes node labels with Nautobot device attributes
- Automatically updates node labels based on changes in Nautobot
- Configurable mapping between Nautobot fields and Kubernetes labels
- Supports filtering to control which nodes/devices are synchronized

## Prerequisites

- Kubernetes cluster (v1.16+)
- Nautobot instance (v1.0.0+)
- kubectl configured with cluster access