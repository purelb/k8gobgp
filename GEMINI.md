# Project: BGP implementation for kubernetes based upon gobgp


- You are an expert software developer in go, kubernetes and networking


## Overview
This project integrates gobgp with Kubernetes.


## Instructions


## Resources.
* The gobgp source is located at /home/adamd/go/gobgp

* all code is to be written in go.

* The external container registry is ghcr.io/adamdunstan/registry

* Use the kubernetes cluster currently accessible via kubectl

## Instructions

1. Analyze the code base osrg/gobpg.  Use the gobgp mcp to understand the code base.
   - Analyzed the folder structure of `osrg/gobgp`.
   - Identified `api/gobgp.proto` as the primary API definition.
   - Understood that "gobgp mcp" likely refers to the overall control plane exposed via the gRPC API.

2. Create a CRD based that provides configuration of all parameters contained in the the gobgp api. github.com/osrg/gobgp/v3/api
   - Initially attempted to use `protoc-gen-crd` but faced issues with its installation and usage.
   - Switched to a `controller-gen` based approach.
   - Defined Go structs in `api/v1alpha1/types.go` to represent the `BGPConfiguration` CRD, using `runtime.RawExtension` for `gobgpapi` types to handle deepcopy generation.
   - Created `api/v1alpha1/scheme.go` for API registration.
   - Set up a `Makefile` to run `controller-gen` for deepcopy generation, CRD YAML, RBAC, and webhook configurations.
   - Successfully ran `make generate` to create the CRD YAML (`config/crd/bases/gobgp.k8s.io_bgpconfigurations.yaml`) and other generated files.

3. Create reconciler program that will be added to the container that reads the configuration from the CRD and applies it to the gobgpd daemon using grpc and the api github.com/osrg/gobpg/v3/api
   - Manually created `main.go` to set up the controller manager.
   - Created `controllers/bgpconfiguration_controller.go` with a basic reconciler structure.
   - The reconciler fetches the `BGPConfiguration` CR, establishes a gRPC connection to `gobgpd`, and applies the global BGP configuration.
   - Registered the reconciler with the manager in `main.go`.
   - Successfully built the controller binary (`manager`).

4. Create a gobgp container that contains the gobgp binaries and the reconciler program.  Upload the container into the external contain registry
   - Initially attempted to build `gobgpd` and the reconciler in separate Dockerfiles.
   - Due to `run_shell_command` limitations with build contexts, switched to a multi-stage `Dockerfile` that builds both `gobgpd` and the reconciler within a single image.
   - Copied the `gobgp` source code into the `k8gobgp` directory to make the build context self-contained.
   - **Progress:**
     - Resolved network/DNS issue during Docker build by running `go mod tidy` and `go mod vendor` locally, and then using `-mod=vendor` in the Dockerfile.
     - Corrected Go version in Dockerfile (`golang:1.22-alpine` to `golang:1.24-alpine`).
     - Fixed `git` command error in Docker container by explicitly setting `ENTRYPOINT ["/bin/sh", "-c"]` and adjusting `CMD`.
     - Corrected `WORKDIR` placement in Dockerfile to ensure binaries are found.
     - Successfully built and pushed the Docker image to `ghcr.io/adamdunstan/registry/k8gobgp:latest`.

5.  Create a daemonset manifest to deploy the gobgp container on every node. use hostNetwork: true   use the container previously created
    - **Progress:**
      - Created `config/daemonset/gobgp-daemonset.yaml` to deploy the `k8gobgp` container on every node with `hostNetwork: true`.
      - Manually created `ServiceAccount` and `ClusterRoleBinding` manifests in `config/rbac` as `controller-gen` was not generating them reliably.
      - Applied `ClusterRole`, `ServiceAccount`, `ClusterRoleBinding`, and `DaemonSet` manifests to the Kubernetes cluster.

6. Apply the CRD previously create and create a basic gobgp configuration.
   - **Progress:**
     - Fixed `metadata.name` error in CRD by renaming the CRD file and updating `metadata.name` in the YAML to `bgpconfigurations.bgp.example.com`.
     - Changed the CRD group from `gobgp.k8s.io` to `bgp.example.com` in `api/v1alpha1/scheme.go` and the CRD YAML to avoid `api-approved.kubernetes.io` annotation requirement.
     - Created a sample `BGPConfiguration` custom resource (`config/samples/bgpconfiguration-sample.yaml`) with a basic global BGP configuration.
     - Applied the `BGPConfiguration` CRD and the sample `BGPConfiguration` custom resource.
     - **Current Blocker:** Encountering "no kind is registered for the type v1alpha1.BGPConfiguration in scheme" error in controller logs, despite regenerating deepcopy code and rebuilding. This indicates a persistent issue with scheme registration or `DeepCopyInto` methods.
     - **Debugging Steps Taken:**
       - Cleaned Go module cache (`go clean -modcache`).
       - Deleted `zz_generated.deepcopy.go`.
       - Ran `make generate` and `controller-gen object` multiple times.
       - Attempted to manually implement `DeepCopyInto` methods for `BGPConfigurationSpec` and `GlobalSpec` (and nested structs) in `api/v1alpha1/types.go`.
       - Reverted explicit `scheme.AddKnownTypes` call in `main.go`.
       - Recreated `api/v1alpha1/types.go` from scratch to ensure a clean state.
       - **Current Status:** Still facing "no kind is registered" error. The issue seems to be with `controller-gen`'s inability to correctly generate `DeepCopyInto` methods for complex nested structs, or a subtle interaction with the Kubernetes scheme.
