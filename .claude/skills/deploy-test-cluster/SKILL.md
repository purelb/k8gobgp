---
name: deploy-test-cluster
description: Build and deploy PureLB images to a test cluster with ko. Use when deploying, pushing images, or rolling out allocator/lbnodeagent changes to a cluster.
---

# Building and Deploying to Test Cluster

**IMPORTANT**: The default `make image` builds to `ko.local/` which requires local Docker. For deploying to the test cluster, you must use `ko` directly with the correct registry and tag.

### There are multiple custers used for texting. Check the cluster in use
```bash
kubectx
```

### Check Current Cluster Image Tags

First, check what image tags the cluster is currently using:
```bash
kubectl get daemonset lbnodeagent -n purelb-system-o jsonpath='{.spec.template.spec.containers[0].image}'
# Example output: ghcr.io/purelb/purelb/lbnodeagent:general_k8_update
```

### Build and Push with ko

Use `ko` directly with the correct registry (`ghcr.io/purelb/purelb`) and tag (matching the current branch/deployment):
```bash
# Set the registry and TAG (both required - TAG is used by .ko.yaml for ldflags)
export KO_DOCKER_REPO=ghcr.io/purelb/purelb
export TAG=general_k8_update  # Must match the tag you're deploying

# Build and push with the correct tag (match current cluster deployment)
go run github.com/google/ko@v0.17.1 build --base-import-paths --tags=$TAG ./cmd/lbnodeagent
go run github.com/google/ko@v0.17.1 build --base-import-paths --tags=$TAG ./cmd/allocator
```

### Restart Pods to Pick Up New Images

After pushing new images, restart the pods to pull the updated images:
```bash
kubectl rollout restart daemonset/lbnodeagent -n purelb-system
kubectl rollout restart deployment/allocator -n purelb-system

# Wait for rollout to complete
kubectl rollout status daemonset/lbnodeagent -n purelb-system
kubectl rollout status deployment/allocator -n purelb-system
```

### Common Mistakes to Avoid

1. **Don't use `make image`** for cluster deployment - it builds to `ko.local/` which requires local Docker daemon
2. **Always check the current image tag** before building - use the same tag the cluster expects
3. **Remember to restart pods** after pushing - Kubernetes won't automatically pull updated images with the same tag
