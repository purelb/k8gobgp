// Copyright 2025 Acnodal Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controllers

import (
	"context"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	gobgpapi "github.com/osrg/gobgp/v4/api"
	bgpv1 "github.com/purelb/k8gobgp/api/v1"
)

const (
	// finalizerName is the finalizer used to ensure cleanup on deletion
	finalizerName = "bgp.purelb.io/finalizer"

	// defaultGoBGPEndpoint is the default gRPC endpoint for GoBGP
	defaultGoBGPEndpoint = "localhost:50051"

	// Environment variable for configuring GoBGP endpoint
	envGoBGPEndpoint = "GOBGP_ENDPOINT"
)

type BGPConfigurationReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme

	// GoBGPEndpoint is the gRPC endpoint for GoBGP (supports tcp:// or unix://)
	GoBGPEndpoint string

	// Recorder is for emitting Kubernetes Events (router ID resolution, etc.)
	Recorder record.EventRecorder

	// NodeName is the name of the node this controller is running on (from NODE_NAME env)
	NodeName string

	// PodName is this pod's name (from POD_NAME env, used for pod annotations)
	PodName string

	// PodNamespace is this pod's namespace (from POD_NAMESPACE env, used for pod annotations)
	PodNamespace string

	// nodeCache caches Node objects to reduce Kubernetes API calls
	nodeCache *nodeCache

	// localRouterIDCache stores per-BGPConfiguration resolved router IDs for
	// this pod. Keyed by "namespace/name". Used for immutability: once resolved,
	// a config's router ID won't change during the pod's lifetime. This is
	// per-pod (not shared via status) because in a DaemonSet each pod resolves
	// its own node's IP.
	localRouterIDCache map[string]*localRouterIDEntry
}

type localRouterIDEntry struct {
	routerID string
	source   string
}

// +kubebuilder:rbac:groups=bgp.purelb.io,resources=configs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=bgp.purelb.io,resources=configs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=bgp.purelb.io,resources=configs/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *BGPConfigurationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	startTime := time.Now()
	log := r.Log.WithValues("bgpconfiguration", req.NamespacedName)
	bgpConfig := &bgpv1.BGPConfiguration{}
	if err := r.Get(ctx, req.NamespacedName, bgpConfig); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("BGPConfiguration not found, ignoring")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle deletion with finalizer
	if !bgpConfig.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, bgpConfig, log)
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(bgpConfig, finalizerName) {
		controllerutil.AddFinalizer(bgpConfig, finalizerName)
		if err := r.Update(ctx, bgpConfig); err != nil {
			log.Error(err, "Failed to add finalizer")
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	log.Info("Reconciling BGPConfiguration")

	// Validate configuration before attempting to apply
	validationResult := r.validateConfiguration(bgpConfig)
	if !validationResult.Valid {
		errMsg := validationResult.ErrorMessages()
		log.Error(nil, "Configuration validation failed", "errors", errMsg)
		r.updateStatusCondition(ctx, bgpConfig, "Ready", metav1.ConditionFalse, "ConfigurationInvalid", errMsg)
		bgpConfig.Status.ObservedGeneration = bgpConfig.Generation
		bgpConfig.Status.Message = "Configuration validation failed: " + errMsg
		now := metav1.Now()
		bgpConfig.Status.LastReconcileTime = &now
		if err := r.Status().Update(ctx, bgpConfig); err != nil {
			log.Error(err, "Failed to update BGPConfiguration status")
		}
		RecordReconcileResult(bgpConfig.Name, bgpConfig.Namespace, "validation_failed")
		UpdateConfigurationReadyStatus(bgpConfig.Name, bgpConfig.Namespace, false)
		// Don't requeue immediately - wait for user to fix config
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Connect to GoBGP
	conn, err := r.dialGoBGP(ctx)
	if err != nil {
		log.Error(err, "Failed to connect to gobgpd")
		RecordGoBGPConnectionError(r.getEndpoint())
		RecordGoBGPConnection(r.getEndpoint(), false)
		RecordReconcileResult(bgpConfig.Name, bgpConfig.Namespace, "connection_failed")
		r.updateStatusCondition(ctx, bgpConfig, "Ready", metav1.ConditionFalse, "ConnectionFailed", err.Error())
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	defer func() {
		if err := conn.Close(); err != nil {
			log.Error(err, "Failed to close gRPC connection")
		}
	}()
	RecordGoBGPConnection(r.getEndpoint(), true)
	apiClient := gobgpapi.NewGoBgpServiceClient(conn)

	// Reconcile all BGP configuration components
	var reconcileErr error
	if err := r.reconcileGlobal(ctx, apiClient, bgpConfig, log); err != nil {
		reconcileErr = fmt.Errorf("global: %w", err)
	} else if err := r.reconcileDefinedSets(ctx, apiClient, bgpConfig, log); err != nil {
		reconcileErr = fmt.Errorf("defined sets: %w", err)
	} else if err := r.reconcilePolicies(ctx, apiClient, bgpConfig, log); err != nil {
		reconcileErr = fmt.Errorf("policies: %w", err)
	} else if err := r.reconcileVrfs(ctx, apiClient, bgpConfig, log); err != nil {
		reconcileErr = fmt.Errorf("vrfs: %w", err)
	} else if err := r.reconcilePeerGroups(ctx, apiClient, bgpConfig, log); err != nil {
		reconcileErr = fmt.Errorf("peer groups: %w", err)
	} else if err := r.reconcileDynamicNeighbors(ctx, apiClient, bgpConfig, log); err != nil {
		reconcileErr = fmt.Errorf("dynamic neighbors: %w", err)
	} else if err := r.reconcileNeighbors(ctx, apiClient, bgpConfig, log); err != nil {
		reconcileErr = fmt.Errorf("neighbors: %w", err)
	} else if err := r.reconcileNetlink(ctx, apiClient, bgpConfig, log); err != nil {
		reconcileErr = fmt.Errorf("netlink: %w", err)
	}

	// Update status
	bgpConfig.Status.ObservedGeneration = bgpConfig.Generation
	bgpConfig.Status.NeighborCount = len(bgpConfig.Spec.Neighbors)
	now := metav1.Now()
	bgpConfig.Status.LastReconcileTime = &now

	// Update configuration metrics
	UpdateNeighborMetrics(bgpConfig.Name, bgpConfig.Namespace, len(bgpConfig.Spec.Neighbors), bgpConfig.Status.EstablishedNeighbors)
	UpdatePeerGroupMetrics(bgpConfig.Name, bgpConfig.Namespace, len(bgpConfig.Spec.PeerGroups))
	UpdateDynamicNeighborMetrics(bgpConfig.Name, bgpConfig.Namespace, len(bgpConfig.Spec.DynamicNeighbors))
	UpdateVrfMetrics(bgpConfig.Name, bgpConfig.Namespace, len(bgpConfig.Spec.Vrfs))
	UpdatePolicyMetrics(bgpConfig.Name, bgpConfig.Namespace, len(bgpConfig.Spec.PolicyDefinitions), len(bgpConfig.Spec.DefinedSets))

	if reconcileErr != nil {
		log.Error(reconcileErr, "Reconciliation failed")
		r.updateStatusCondition(ctx, bgpConfig, "Ready", metav1.ConditionFalse, "ReconcileFailed", reconcileErr.Error())
		bgpConfig.Status.Message = reconcileErr.Error()
		if err := r.Status().Update(ctx, bgpConfig); err != nil {
			log.Error(err, "Failed to update BGPConfiguration status")
		}
		RecordReconcileResult(bgpConfig.Name, bgpConfig.Namespace, "failed")
		RecordReconcileDuration(bgpConfig.Name, bgpConfig.Namespace, time.Since(startTime).Seconds())
		UpdateConfigurationReadyStatus(bgpConfig.Name, bgpConfig.Namespace, false)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	r.updateStatusCondition(ctx, bgpConfig, "Ready", metav1.ConditionTrue, "ReconcileSuccess", "BGP configuration applied successfully")
	bgpConfig.Status.Message = "BGP configuration applied successfully"
	if err := r.Status().Update(ctx, bgpConfig); err != nil {
		log.Error(err, "Failed to update BGPConfiguration status")
		return ctrl.Result{}, err
	}

	RecordReconcileResult(bgpConfig.Name, bgpConfig.Namespace, "success")
	RecordReconcileDuration(bgpConfig.Name, bgpConfig.Namespace, time.Since(startTime).Seconds())
	UpdateConfigurationReadyStatus(bgpConfig.Name, bgpConfig.Namespace, true)

	log.Info("Successfully reconciled BGPConfiguration")
	return ctrl.Result{}, nil
}

// dialGoBGP establishes a gRPC connection to the GoBGP daemon
func (r *BGPConfigurationReconciler) dialGoBGP(_ context.Context) (*grpc.ClientConn, error) {
	endpoint := r.GoBGPEndpoint
	if endpoint == "" {
		endpoint = os.Getenv(envGoBGPEndpoint)
	}
	if endpoint == "" {
		endpoint = defaultGoBGPEndpoint
	}

	// Support Unix socket connections (unix:///path/to/socket)
	if strings.HasPrefix(endpoint, "unix://") {
		socketPath := strings.TrimPrefix(endpoint, "unix://")
		return grpc.NewClient("unix:"+socketPath,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
	}

	// Default TCP connection
	return grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
}

// getEndpoint returns the configured GoBGP endpoint for metrics
func (r *BGPConfigurationReconciler) getEndpoint() string {
	endpoint := r.GoBGPEndpoint
	if endpoint == "" {
		endpoint = os.Getenv(envGoBGPEndpoint)
	}
	if endpoint == "" {
		endpoint = defaultGoBGPEndpoint
	}
	return endpoint
}

// reconcileDelete handles cleanup when BGPConfiguration is deleted
func (r *BGPConfigurationReconciler) reconcileDelete(ctx context.Context, bgpConfig *bgpv1.BGPConfiguration, log logr.Logger) (ctrl.Result, error) {
	log.Info("Handling deletion of BGPConfiguration")
	cleanupStart := time.Now()

	if controllerutil.ContainsFinalizer(bgpConfig, finalizerName) {
		// Cleanup BGP configuration with retry logic
		conn, err := r.dialGoBGP(ctx)
		if err != nil {
			log.Error(err, "Failed to connect to gobgpd for cleanup, will retry")
			RecordCleanupRetry(bgpConfig.Name, bgpConfig.Namespace)
			// Retry after delay if connection fails - don't remove finalizer yet
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		defer func() {
			if err := conn.Close(); err != nil {
				log.Error(err, "Failed to close gRPC connection during cleanup")
			}
		}()
		apiClient := gobgpapi.NewGoBgpServiceClient(conn)

		// Track cleanup errors
		var cleanupErrors []error

		// Delete neighbors first (must be done before peer groups)
		for _, n := range bgpConfig.Spec.Neighbors {
			if _, err := apiClient.DeletePeer(ctx, &gobgpapi.DeletePeerRequest{Address: n.Config.NeighborAddress}); err != nil {
				log.Error(err, "Failed to delete neighbor during cleanup", "address", n.Config.NeighborAddress)
				cleanupErrors = append(cleanupErrors, err)
			}
		}

		// Delete dynamic neighbors
		for _, dn := range bgpConfig.Spec.DynamicNeighbors {
			if _, err := apiClient.DeleteDynamicNeighbor(ctx, &gobgpapi.DeleteDynamicNeighborRequest{Prefix: dn.Prefix}); err != nil {
				log.Error(err, "Failed to delete dynamic neighbor during cleanup", "prefix", dn.Prefix)
				cleanupErrors = append(cleanupErrors, err)
			}
		}

		// Delete peer groups
		for _, pg := range bgpConfig.Spec.PeerGroups {
			if _, err := apiClient.DeletePeerGroup(ctx, &gobgpapi.DeletePeerGroupRequest{Name: pg.Config.PeerGroupName}); err != nil {
				log.Error(err, "Failed to delete peer group during cleanup", "name", pg.Config.PeerGroupName)
				cleanupErrors = append(cleanupErrors, err)
			}
		}

		// Delete VRFs
		for _, vrf := range bgpConfig.Spec.Vrfs {
			if _, err := apiClient.DeleteVrf(ctx, &gobgpapi.DeleteVrfRequest{Name: vrf.Name}); err != nil {
				log.Error(err, "Failed to delete VRF during cleanup", "name", vrf.Name)
				cleanupErrors = append(cleanupErrors, err)
			}
		}

		// Delete policies
		for _, policy := range bgpConfig.Spec.PolicyDefinitions {
			if _, err := apiClient.DeletePolicy(ctx, &gobgpapi.DeletePolicyRequest{Policy: &gobgpapi.Policy{Name: policy.Name}}); err != nil {
				log.Error(err, "Failed to delete policy during cleanup", "name", policy.Name)
				cleanupErrors = append(cleanupErrors, err)
			}
		}

		// Delete defined sets
		for _, set := range bgpConfig.Spec.DefinedSets {
			if _, err := apiClient.DeleteDefinedSet(ctx, &gobgpapi.DeleteDefinedSetRequest{DefinedSet: &gobgpapi.DefinedSet{Name: set.Name}}); err != nil {
				log.Error(err, "Failed to delete defined set during cleanup", "name", set.Name)
				cleanupErrors = append(cleanupErrors, err)
			}
		}

		// Note: We don't call StopBgp here because:
		// 1. StopBgp can block indefinitely waiting for peer FSMs to complete
		// 2. The BGP server will stop when the pod terminates
		// 3. We've already cleaned up neighbors, peer groups, VRFs, and other resources

		// If there were cleanup errors, retry
		if len(cleanupErrors) > 0 {
			log.Info("Some cleanup operations failed, will retry", "errorCount", len(cleanupErrors))
			RecordCleanupRetry(bgpConfig.Name, bgpConfig.Namespace)
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}

		// Record successful cleanup metrics
		RecordCleanupDuration(bgpConfig.Name, bgpConfig.Namespace, time.Since(cleanupStart).Seconds())
		// Delete all metrics for this configuration
		DeleteMetricsForConfig(bgpConfig.Name, bgpConfig.Namespace)

		// Remove finalizer only after successful cleanup
		controllerutil.RemoveFinalizer(bgpConfig, finalizerName)
		if err := r.Update(ctx, bgpConfig); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// updateStatusCondition updates a condition on the BGPConfiguration status.
// Always updates Reason and Message. Only updates LastTransitionTime when
// the Status value changes (per Kubernetes API conventions).
func (r *BGPConfigurationReconciler) updateStatusCondition(ctx context.Context, bgpConfig *bgpv1.BGPConfiguration, condType string, status metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()

	// Find existing condition
	for i, c := range bgpConfig.Status.Conditions {
		if c.Type == condType {
			bgpConfig.Status.Conditions[i].ObservedGeneration = bgpConfig.Generation
			bgpConfig.Status.Conditions[i].Reason = reason
			bgpConfig.Status.Conditions[i].Message = message
			if c.Status != status {
				bgpConfig.Status.Conditions[i].Status = status
				bgpConfig.Status.Conditions[i].LastTransitionTime = now
			}
			return
		}
	}

	// Condition not found — append new one
	bgpConfig.Status.Conditions = append(bgpConfig.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		ObservedGeneration: bgpConfig.Generation,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            message,
	})
}

// validateConfiguration validates the BGPConfiguration before applying it
// Returns a ValidationResult with any errors found
func (r *BGPConfigurationReconciler) validateConfiguration(bgpConfig *bgpv1.BGPConfiguration) *ValidationResult {
	result := &ValidationResult{Valid: true}

	// Validate routerIDPool if specified
	if bgpConfig.Spec.Global.RouterIDPool != "" {
		if err := ValidateRouterIDPool(bgpConfig.Spec.Global.RouterIDPool); err != nil {
			result.AddError("spec.global.routerIDPool", bgpConfig.Spec.Global.RouterIDPool, err.Error())
		}
	}

	// Validate global netlink export rules
	if bgpConfig.Spec.NetlinkExport != nil {
		for i, rule := range bgpConfig.Spec.NetlinkExport.Rules {
			fieldPrefix := fmt.Sprintf("spec.netlinkExport.rules[%d]", i)

			// Validate community list
			for _, community := range rule.CommunityList {
				if err := ValidateCommunity(community); err != nil {
					result.AddError(fieldPrefix+".communityList", community, err.Error())
				}
			}

			// Validate large community list
			for _, community := range rule.LargeCommunityList {
				if err := ValidateLargeCommunity(community); err != nil {
					result.AddError(fieldPrefix+".largeCommunityList", community, err.Error())
				}
			}
		}
	}

	// Validate VRF netlink export configurations
	for i, vrf := range bgpConfig.Spec.Vrfs {
		if vrf.NetlinkExport != nil {
			fieldPrefix := fmt.Sprintf("spec.vrfs[%d].netlinkExport", i)

			// Validate community list
			for _, community := range vrf.NetlinkExport.CommunityList {
				if err := ValidateCommunity(community); err != nil {
					result.AddError(fieldPrefix+".communityList", community, err.Error())
				}
			}

			// Validate large community list
			for _, community := range vrf.NetlinkExport.LargeCommunityList {
				if err := ValidateLargeCommunity(community); err != nil {
					result.AddError(fieldPrefix+".largeCommunityList", community, err.Error())
				}
			}
		}
	}

	// Validate community actions in policy definitions
	for i, policy := range bgpConfig.Spec.PolicyDefinitions {
		for j, stmt := range policy.Statements {
			if stmt.Actions.Community != nil {
				fieldPrefix := fmt.Sprintf("spec.policyDefinitions[%d].statements[%d].actions.community", i, j)
				for _, community := range stmt.Actions.Community.Communities {
					if err := ValidateCommunity(community); err != nil {
						result.AddError(fieldPrefix+".communities", community, err.Error())
					}
				}
			}
		}
	}

	// Validate community defined sets
	for i, set := range bgpConfig.Spec.DefinedSets {
		if set.Type == "community" {
			fieldPrefix := fmt.Sprintf("spec.definedSets[%d]", i)
			for _, community := range set.List {
				if err := ValidateCommunity(community); err != nil {
					result.AddError(fieldPrefix+".list", community, err.Error())
				}
			}
		}
	}

	return result
}

func (r *BGPConfigurationReconciler) reconcileGlobal(ctx context.Context, apiClient gobgpapi.GoBgpServiceClient, bgpConfig *bgpv1.BGPConfiguration, log logr.Logger) error {
	// Resolve the router ID (with immutability - uses status if already resolved)
	effectiveRouterID, err := r.resolveEffectiveRouterID(ctx, bgpConfig, log)
	if err != nil {
		r.emitRouterIDFailedEvent(bgpConfig, err)
		r.updateStatusCondition(ctx, bgpConfig, "RouterIDResolved", metav1.ConditionFalse,
			"ResolutionFailed", fmt.Sprintf("Router ID resolution failed: %s", err.Error()))
		return fmt.Errorf("failed to resolve router ID: %w", err)
	}

	// Create desired config with resolved router ID
	desired := &gobgpapi.Global{
		Asn:             bgpConfig.Spec.Global.ASN,
		RouterId:        effectiveRouterID,
		ListenPort:      bgpConfig.Spec.Global.ListenPort,
		ListenAddresses: bgpConfig.Spec.Global.ListenAddresses,
	}

	current, err := apiClient.GetBgp(ctx, &gobgpapi.GetBgpRequest{})
	if err != nil {
		// gRPC error - server may not be accessible
		log.Info("BGP server not running, calling StartBgp", "routerID", effectiveRouterID)
		_, startErr := apiClient.StartBgp(ctx, &gobgpapi.StartBgpRequest{Global: desired})
		return startErr
	}

	// GetBgp returns empty config (ASN=0) when gobgpd started but StartBgp never called
	if current.Global.Asn == 0 {
		log.Info("BGP server not initialized, calling StartBgp", "routerID", effectiveRouterID)
		_, startErr := apiClient.StartBgp(ctx, &gobgpapi.StartBgpRequest{Global: desired})
		return startErr
	}

	// BGP server already running with config - check if immutable fields (ASN, RouterID) match
	// We only compare ASN and RouterID because other fields (ListenPort, ListenAddresses)
	// are set by gobgp with defaults and would cause false positives with reflect.DeepEqual
	if current.Global.Asn != desired.Asn || current.Global.RouterId != desired.RouterId {
		// Global config (ASN, RouterID) cannot be changed dynamically in gobgp.
		// A pod restart is required to apply changes to global configuration.
		log.Info("Global config changed - pod restart required to apply changes",
			"currentASN", current.Global.Asn,
			"desiredASN", desired.Asn,
			"currentRouterID", current.Global.RouterId,
			"desiredRouterID", desired.RouterId)
		return fmt.Errorf("global configuration change detected (ASN or RouterID); pod restart required to apply")
	}
	return nil
}

// resolveEffectiveRouterID resolves the router ID, respecting immutability.
// Uses a per-BGPConfiguration in-memory cache so each config gets its own
// resolved ID, and the ID doesn't change during the pod's lifetime.
func (r *BGPConfigurationReconciler) resolveEffectiveRouterID(ctx context.Context, bgpConfig *bgpv1.BGPConfiguration, log logr.Logger) (string, error) {
	cacheKey := bgpConfig.Namespace + "/" + bgpConfig.Name

	// Initialize cache map on first use
	if r.localRouterIDCache == nil {
		r.localRouterIDCache = make(map[string]*localRouterIDEntry)
	}

	// Immutability: if already resolved in-memory for this config on this pod, reuse it.
	// We use an in-memory cache (not the shared status) because in a DaemonSet
	// each pod reconciles the same BGPConfiguration but needs its own node's router ID.
	if entry, ok := r.localRouterIDCache[cacheKey]; ok {
		log.V(1).Info("using previously resolved router ID (immutable, in-memory)",
			"routerID", entry.routerID,
			"source", entry.source,
			"nodeName", r.NodeName)

		// Check if configured value differs from locked value
		if bgpConfig.Spec.Global.RouterID != "" &&
			bgpConfig.Spec.Global.RouterID != entry.routerID &&
			!strings.HasPrefix(bgpConfig.Spec.Global.RouterID, "${") {
			log.Info("WARNING: configured routerID differs from locked value - pod restart required to change",
				"configured", bgpConfig.Spec.Global.RouterID,
				"locked", entry.routerID)
		}

		// Set shared status field (only resolution method, not per-node details).
		// In a DaemonSet, each pod resolves its own node's IP, so per-node details
		// are available via pod annotations, metrics, and Kubernetes events instead.
		bgpConfig.Status.RouterIDSource = entry.source

		return entry.routerID, nil
	}

	// Resolve the router ID
	startTime := time.Now()
	resolution, err := r.ResolveRouterID(ctx, &bgpConfig.Spec.Global, log)
	duration := time.Since(startTime).Seconds()

	if err != nil {
		RecordRouterIDResolution("failure", duration)
		return "", err
	}

	// Record metrics
	RecordRouterIDResolution("success", duration)
	UpdateRouterIDSource(resolution.Source)
	UpdateRouterIDInfo(resolution.RouterID, resolution.Source, resolution.NodeName, fmt.Sprintf("%d", bgpConfig.Spec.Global.ASN))

	// Cache in-memory for immutability (per-config, per-pod)
	r.localRouterIDCache[cacheKey] = &localRouterIDEntry{
		routerID: resolution.RouterID,
		source:   resolution.Source,
	}

	// Set shared status fields. Only write resolution method and timestamp
	// to the shared status — not per-node details which would be misleading
	// in a DaemonSet where each pod has a different value. Per-node router
	// ID details are available via:
	//   - Pod annotations: bgp.purelb.io/router-id, bgp.purelb.io/asn
	//   - Prometheus metrics: k8gobgp_router_id_info{router_id, source, node, asn}
	//   - Kubernetes events: RouterIDResolved
	now := metav1.Now()
	bgpConfig.Status.RouterIDSource = resolution.Source
	bgpConfig.Status.RouterIDResolutionTime = &now

	// Set a RouterIDResolved condition (generic, not per-node)
	r.updateStatusCondition(ctx, bgpConfig, "RouterIDResolved", metav1.ConditionTrue,
		"Resolved", fmt.Sprintf("Router ID resolved via %s", resolution.Source))

	// Emit per-pod event (includes specific router ID and node for debugging)
	r.emitRouterIDResolvedEvent(bgpConfig, resolution)

	// Annotate this pod with the resolved router ID and ASN for per-pod visibility
	if err := r.annotatePodWithRouterID(ctx, resolution, bgpConfig.Spec.Global.ASN); err != nil {
		log.Error(err, "failed to annotate pod with router ID (non-fatal)")
	}

	log.Info("router ID resolved",
		"routerID", resolution.RouterID,
		"source", resolution.Source,
		"nodeName", resolution.NodeName,
		"duration", duration)

	return resolution.RouterID, nil
}

func (r *BGPConfigurationReconciler) reconcileDefinedSets(ctx context.Context, apiClient gobgpapi.GoBgpServiceClient, bgpConfig *bgpv1.BGPConfiguration, log logr.Logger) error {
	// 1. Get current defined sets
	currentSets := make(map[string]*gobgpapi.DefinedSet)
	stream, err := apiClient.ListDefinedSet(ctx, &gobgpapi.ListDefinedSetRequest{})
	if err != nil {
		return err
	}
	for {
		res, err := stream.Recv()
		if err != nil {
			if err != io.EOF {
				log.Error(err, "error listing defined sets from gobgpd")
			}
			break
		}
		currentSets[res.DefinedSet.Name] = res.DefinedSet
	}

	// 2. Get desired defined sets
	desiredSets := make(map[string]*gobgpapi.DefinedSet)
	for _, set := range bgpConfig.Spec.DefinedSets {
		desiredSets[set.Name] = crdToAPIDefinedSet(&set)
	}

	// 3. Delete unwanted sets
	for name := range currentSets {
		if _, ok := desiredSets[name]; !ok {
			log.Info("Deleting defined set", "name", name)
			if _, err := apiClient.DeleteDefinedSet(ctx, &gobgpapi.DeleteDefinedSetRequest{DefinedSet: &gobgpapi.DefinedSet{Name: name}}); err != nil {
				log.Error(err, "Failed to delete defined set", "name", name)
			}
		}
	}

	// 4. Add or update sets
	for name, desired := range desiredSets {
		if current, ok := currentSets[name]; !ok {
			log.Info("Adding defined set", "name", name)
			if _, err := apiClient.AddDefinedSet(ctx, &gobgpapi.AddDefinedSetRequest{DefinedSet: desired}); err != nil {
				log.Error(err, "Failed to add defined set", "name", name)
			}
		} else {
			if !reflect.DeepEqual(desired, current) {
				log.Info("Updating defined set", "name", name)
				if _, err := apiClient.AddDefinedSet(ctx, &gobgpapi.AddDefinedSetRequest{DefinedSet: desired, Replace: true}); err != nil {
					log.Error(err, "Failed to update defined set", "name", name)
				}
			}
		}
	}
	return nil
}

func (r *BGPConfigurationReconciler) reconcilePolicies(ctx context.Context, apiClient gobgpapi.GoBgpServiceClient, bgpConfig *bgpv1.BGPConfiguration, log logr.Logger) error {
	// 1. Get current policies
	currentPolicies := make(map[string]*gobgpapi.Policy)
	stream, err := apiClient.ListPolicy(ctx, &gobgpapi.ListPolicyRequest{})
	if err != nil {
		return err
	}
	for {
		res, err := stream.Recv()
		if err != nil {
			if err != io.EOF {
				log.Error(err, "error listing policies from gobgpd")
			}
			break
		}
		currentPolicies[res.Policy.Name] = res.Policy
	}

	// 2. Get desired policies
	desiredPolicies := make(map[string]*gobgpapi.Policy)
	for _, policy := range bgpConfig.Spec.PolicyDefinitions {
		desiredPolicies[policy.Name] = crdToAPIPolicy(&policy)
	}

	// 3. Delete unwanted policies
	for name := range currentPolicies {
		if _, ok := desiredPolicies[name]; !ok {
			log.Info("Deleting policy", "name", name)
			if _, err := apiClient.DeletePolicy(ctx, &gobgpapi.DeletePolicyRequest{Policy: &gobgpapi.Policy{Name: name}}); err != nil {
				log.Error(err, "Failed to delete policy", "name", name)
			}
		}
	}

	// 4. Add or update policies
	for name, desired := range desiredPolicies {
		// AddPolicy with the same name will update the existing policy
		log.Info("Adding/Updating policy", "name", name)
		if _, err := apiClient.AddPolicy(ctx, &gobgpapi.AddPolicyRequest{Policy: desired}); err != nil {
			log.Error(err, "Failed to add/update policy", "name", name)
		}
	}
	return nil
}

func (r *BGPConfigurationReconciler) reconcileVrfs(ctx context.Context, apiClient gobgpapi.GoBgpServiceClient, bgpConfig *bgpv1.BGPConfiguration, log logr.Logger) error {
	// 1. Get current VRFs
	currentVrfs := make(map[string]*gobgpapi.Vrf)
	stream, err := apiClient.ListVrf(ctx, &gobgpapi.ListVrfRequest{})
	if err != nil {
		return err
	}
	for {
		res, err := stream.Recv()
		if err != nil {
			if err != io.EOF {
				log.Error(err, "error listing VRFs from gobgpd")
			}
			break
		}
		currentVrfs[res.Vrf.Name] = res.Vrf
	}

	// 2. Get desired VRFs
	desiredVrfs := make(map[string]*gobgpapi.Vrf)
	for _, vrf := range bgpConfig.Spec.Vrfs {
		desiredVrfs[vrf.Name] = crdToAPIVrf(&vrf)
	}

	// 3. Delete unwanted VRFs
	for name := range currentVrfs {
		if _, ok := desiredVrfs[name]; !ok {
			log.Info("Deleting VRF", "name", name)
			if _, err := apiClient.DeleteVrf(ctx, &gobgpapi.DeleteVrfRequest{Name: name}); err != nil {
				log.Error(err, "Failed to delete VRF", "name", name)
			}
		}
	}

	// 4. Add or update VRFs
	for name, desired := range desiredVrfs {
		if _, ok := currentVrfs[name]; !ok {
			log.Info("Adding VRF", "name", name)
			if _, err := apiClient.AddVrf(ctx, &gobgpapi.AddVrfRequest{Vrf: desired}); err != nil {
				log.Error(err, "Failed to add VRF", "name", name)
			}
		}
		// Note: GoBGP doesn't support UpdateVrf, would need delete+add
	}
	return nil
}

func (r *BGPConfigurationReconciler) reconcileDynamicNeighbors(ctx context.Context, apiClient gobgpapi.GoBgpServiceClient, bgpConfig *bgpv1.BGPConfiguration, log logr.Logger) error {
	// 1. Get current dynamic neighbors
	currentDynNeighbors := make(map[string]*gobgpapi.DynamicNeighbor)
	stream, err := apiClient.ListDynamicNeighbor(ctx, &gobgpapi.ListDynamicNeighborRequest{})
	if err != nil {
		return err
	}
	for {
		res, err := stream.Recv()
		if err != nil {
			if err != io.EOF {
				log.Error(err, "error listing dynamic neighbors from gobgpd")
			}
			break
		}
		currentDynNeighbors[res.DynamicNeighbor.Prefix] = res.DynamicNeighbor
	}

	// 2. Get desired dynamic neighbors
	desiredDynNeighbors := make(map[string]*gobgpapi.DynamicNeighbor)
	for _, dn := range bgpConfig.Spec.DynamicNeighbors {
		desiredDynNeighbors[dn.Prefix] = &gobgpapi.DynamicNeighbor{
			Prefix:    dn.Prefix,
			PeerGroup: dn.PeerGroup,
		}
	}

	// 3. Delete unwanted dynamic neighbors
	for prefix := range currentDynNeighbors {
		if _, ok := desiredDynNeighbors[prefix]; !ok {
			log.Info("Deleting dynamic neighbor", "prefix", prefix)
			if _, err := apiClient.DeleteDynamicNeighbor(ctx, &gobgpapi.DeleteDynamicNeighborRequest{Prefix: prefix}); err != nil {
				log.Error(err, "Failed to delete dynamic neighbor", "prefix", prefix)
			}
		}
	}

	// 4. Add dynamic neighbors (no update needed, delete+add if changed)
	for prefix, desired := range desiredDynNeighbors {
		if current, ok := currentDynNeighbors[prefix]; !ok {
			log.Info("Adding dynamic neighbor", "prefix", prefix)
			if _, err := apiClient.AddDynamicNeighbor(ctx, &gobgpapi.AddDynamicNeighborRequest{DynamicNeighbor: desired}); err != nil {
				log.Error(err, "Failed to add dynamic neighbor", "prefix", prefix)
			}
		} else if current.PeerGroup != desired.PeerGroup {
			// Need to delete and recreate if peer group changed
			log.Info("Updating dynamic neighbor", "prefix", prefix)
			if _, err := apiClient.DeleteDynamicNeighbor(ctx, &gobgpapi.DeleteDynamicNeighborRequest{Prefix: prefix}); err != nil {
				log.Error(err, "Failed to delete dynamic neighbor for update", "prefix", prefix)
			}
			if _, err := apiClient.AddDynamicNeighbor(ctx, &gobgpapi.AddDynamicNeighborRequest{DynamicNeighbor: desired}); err != nil {
				log.Error(err, "Failed to add dynamic neighbor for update", "prefix", prefix)
			}
		}
	}
	return nil
}

func (r *BGPConfigurationReconciler) reconcilePeerGroups(ctx context.Context, apiClient gobgpapi.GoBgpServiceClient, bgpConfig *bgpv1.BGPConfiguration, log logr.Logger) error {
	// 1. Get current peer groups
	currentPeerGroups := make(map[string]*gobgpapi.PeerGroup)
	stream, err := apiClient.ListPeerGroup(ctx, &gobgpapi.ListPeerGroupRequest{})
	if err != nil {
		return err
	}
	for {
		res, err := stream.Recv()
		if err != nil {
			if err != io.EOF {
				log.Error(err, "error listing peer groups from gobgpd")
			}
			break
		}
		currentPeerGroups[res.PeerGroup.Conf.PeerGroupName] = res.PeerGroup
	}

	// 2. Get desired peer groups (with password resolution from Secrets)
	desiredPeerGroups := make(map[string]*gobgpapi.PeerGroup)
	for _, pg := range bgpConfig.Spec.PeerGroups {
		// Resolve auth password from Secret or inline value
		authPassword, err := r.resolveAuthPassword(ctx, bgpConfig.Namespace, pg.Config.AuthPassword, pg.Config.AuthPasswordSecretRef, log)
		if err != nil {
			return fmt.Errorf("peer group %q: %w", pg.Config.PeerGroupName, err)
		}
		desiredPeerGroups[pg.Config.PeerGroupName] = r.crdToAPIPeerGroupWithPassword(&pg, authPassword)
	}

	// 3. Delete unwanted peer groups
	for name := range currentPeerGroups {
		if _, ok := desiredPeerGroups[name]; !ok {
			log.Info("Deleting peer group", "name", name)
			if _, err := apiClient.DeletePeerGroup(ctx, &gobgpapi.DeletePeerGroupRequest{Name: name}); err != nil {
				log.Error(err, "Failed to delete peer group", "name", name)
			}
		}
	}

	// 4. Add or update peer groups
	for name, desired := range desiredPeerGroups {
		if current, ok := currentPeerGroups[name]; !ok {
			log.Info("Adding peer group", "name", name)
			if _, err := apiClient.AddPeerGroup(ctx, &gobgpapi.AddPeerGroupRequest{PeerGroup: desired}); err != nil {
				log.Error(err, "Failed to add peer group", "name", name)
			}
		} else {
			if !reflect.DeepEqual(desired, current) {
				log.Info("Updating peer group", "name", name)
				if _, err := apiClient.UpdatePeerGroup(ctx, &gobgpapi.UpdatePeerGroupRequest{PeerGroup: desired}); err != nil {
					log.Error(err, "Failed to update peer group", "name", name)
				}
			}
		}
	}
	return nil
}

func (r *BGPConfigurationReconciler) reconcileNeighbors(ctx context.Context, apiClient gobgpapi.GoBgpServiceClient, bgpConfig *bgpv1.BGPConfiguration, log logr.Logger) error {
	// 1. Get current neighbors
	currentNeighbors := make(map[string]*gobgpapi.Peer)
	stream, err := apiClient.ListPeer(ctx, &gobgpapi.ListPeerRequest{})
	if err != nil {
		return err
	}
	for {
		res, err := stream.Recv()
		if err != nil {
			if err != io.EOF {
				log.Error(err, "error listing neighbors from gobgpd")
			}
			break
		}
		currentNeighbors[res.Peer.Conf.NeighborAddress] = res.Peer
	}

	// 2. Get desired neighbors (with password resolution from Secrets)
	desiredNeighbors := make(map[string]*gobgpapi.Peer)
	for _, n := range bgpConfig.Spec.Neighbors {
		// Resolve auth password from Secret or inline value
		authPassword, err := r.resolveAuthPassword(ctx, bgpConfig.Namespace, n.Config.AuthPassword, n.Config.AuthPasswordSecretRef, log)
		if err != nil {
			return fmt.Errorf("neighbor %q: %w", n.Config.NeighborAddress, err)
		}
		desiredNeighbors[n.Config.NeighborAddress] = r.crdToAPINeighborWithPassword(&n, authPassword)
	}

	// 3. Delete unwanted neighbors
	for addr := range currentNeighbors {
		if _, ok := desiredNeighbors[addr]; !ok {
			log.Info("Deleting neighbor", "address", addr)
			if _, err := apiClient.DeletePeer(ctx, &gobgpapi.DeletePeerRequest{Address: addr}); err != nil {
				log.Error(err, "Failed to delete neighbor", "address", addr)
			}
		}
	}

	// 4. Add or update neighbors
	for addr, desired := range desiredNeighbors {
		if current, ok := currentNeighbors[addr]; !ok {
			log.Info("Adding neighbor", "address", addr)
			if _, err := apiClient.AddPeer(ctx, &gobgpapi.AddPeerRequest{Peer: desired}); err != nil {
				log.Error(err, "Failed to add neighbor", "address", addr)
			}
		} else {
			if !reflect.DeepEqual(desired, current) {
				log.Info("Updating neighbor", "address", addr)
				if _, err := apiClient.UpdatePeer(ctx, &gobgpapi.UpdatePeerRequest{Peer: desired}); err != nil {
					log.Error(err, "Failed to update neighbor", "address", addr)
				}
			}
		}
	}
	return nil
}

func (r *BGPConfigurationReconciler) reconcileNetlink(ctx context.Context, apiClient gobgpapi.GoBgpServiceClient, bgpConfig *bgpv1.BGPConfiguration, log logr.Logger) error {
	// Get current netlink state
	currentNetlink, err := apiClient.GetNetlink(ctx, &gobgpapi.GetNetlinkRequest{})
	if err != nil {
		log.Error(err, "Failed to get netlink state")
		return err
	}

	// Determine desired state
	desiredImportEnabled := bgpConfig.Spec.NetlinkImport != nil && bgpConfig.Spec.NetlinkImport.Enabled
	desiredExportEnabled := bgpConfig.Spec.NetlinkExport != nil && bgpConfig.Spec.NetlinkExport.Enabled

	var desiredInterfaces []string
	var desiredVrf string
	if bgpConfig.Spec.NetlinkImport != nil {
		desiredInterfaces = bgpConfig.Spec.NetlinkImport.InterfaceList
		desiredVrf = bgpConfig.Spec.NetlinkImport.Vrf
	}

	// Check if we need to update netlink import configuration
	needsImportUpdate := false
	if desiredImportEnabled != currentNetlink.ImportEnabled {
		needsImportUpdate = true
	}
	if desiredImportEnabled && !reflect.DeepEqual(desiredInterfaces, currentNetlink.Interfaces) {
		needsImportUpdate = true
	}

	// Handle netlink import configuration
	if needsImportUpdate {
		if desiredImportEnabled {
			log.Info("Enabling netlink import",
				"vrf", desiredVrf,
				"interfaces", desiredInterfaces)

			_, err := apiClient.EnableNetlinkImport(ctx, &gobgpapi.EnableNetlinkImportRequest{
				Vrf:        desiredVrf,
				Interfaces: desiredInterfaces,
			})
			if err != nil {
				log.Error(err, "Failed to enable netlink import")
				return err
			}
			log.Info("Enabled netlink import", "vrf", desiredVrf, "interfaces", desiredInterfaces)
		} else if currentNetlink.ImportEnabled {
			// Disable netlink import dynamically
			log.Info("Disabling netlink import",
				"currentlyEnabled", currentNetlink.ImportEnabled,
				"desiredEnabled", desiredImportEnabled)

			_, err := apiClient.DisableNetlinkImport(ctx, &gobgpapi.DisableNetlinkImportRequest{
				KeepRoutes: false, // withdraw imported routes from RIB
			})
			if err != nil {
				log.Error(err, "Failed to disable netlink import")
				return err
			}
			log.Info("Disabled netlink import")
		}
	}

	// Check if we need to update netlink export configuration
	needsExportUpdate := false
	if desiredExportEnabled != currentNetlink.ExportEnabled {
		needsExportUpdate = true
	}

	// If export is enabled, also check if the rules have changed
	if desiredExportEnabled && currentNetlink.ExportEnabled && !needsExportUpdate {
		// Get current export rules from gobgpd
		currentRulesResp, err := apiClient.ListNetlinkExportRules(ctx, &gobgpapi.ListNetlinkExportRulesRequest{})
		if err != nil {
			log.Error(err, "Failed to list current netlink export rules")
			return err
		}

		// Compare with desired rules
		desiredRules := bgpConfig.Spec.NetlinkExport.Rules
		if !netlinkExportRulesMatch(currentRulesResp.Rules, desiredRules) {
			log.Info("Netlink export rules changed, updating",
				"currentRuleCount", len(currentRulesResp.Rules),
				"desiredRuleCount", len(desiredRules))
			needsExportUpdate = true
		}
	}

	// Handle netlink export configuration
	if needsExportUpdate {
		if desiredExportEnabled {
			exportConfig := bgpConfig.Spec.NetlinkExport
			log.Info("Enabling netlink export",
				"dampeningInterval", exportConfig.DampeningInterval,
				"routeProtocol", exportConfig.RouteProtocol,
				"rulesCount", len(exportConfig.Rules))

			_, err := apiClient.EnableNetlinkExport(ctx, &gobgpapi.EnableNetlinkExportRequest{
				DampeningInterval: exportConfig.DampeningInterval,
				RouteProtocol:     exportConfig.RouteProtocol,
				Rules:             crdToAPINetlinkExportRules(exportConfig.Rules),
			})
			if err != nil {
				log.Error(err, "Failed to enable netlink export")
				return err
			}
			log.Info("Enabled netlink export", "rulesCount", len(exportConfig.Rules))
		} else if currentNetlink.ExportEnabled {
			// Disable netlink export dynamically
			log.Info("Disabling netlink export",
				"currentlyEnabled", currentNetlink.ExportEnabled,
				"desiredEnabled", desiredExportEnabled)

			_, err := apiClient.DisableNetlinkExport(ctx, &gobgpapi.DisableNetlinkExportRequest{
				KeepRoutes: false, // flush exported routes from Linux kernel
			})
			if err != nil {
				log.Error(err, "Failed to disable netlink export")
				return err
			}
			log.Info("Disabled netlink export")
		}
	}

	// Reconcile VRF-level netlink configurations
	if err := r.reconcileVrfNetlink(ctx, apiClient, bgpConfig, log); err != nil {
		return err
	}

	return nil
}

// reconcileVrfNetlink handles per-VRF netlink import/export configuration
func (r *BGPConfigurationReconciler) reconcileVrfNetlink(ctx context.Context, apiClient gobgpapi.GoBgpServiceClient, bgpConfig *bgpv1.BGPConfiguration, log logr.Logger) error {
	// Get current VRF states from gobgpd
	currentVrfs := make(map[string]*gobgpapi.Vrf)
	stream, err := apiClient.ListVrf(ctx, &gobgpapi.ListVrfRequest{})
	if err != nil {
		log.Error(err, "Failed to list VRFs for netlink reconciliation")
		return err
	}
	for {
		resp, err := stream.Recv()
		if err != nil {
			if err != io.EOF {
				log.Error(err, "error listing VRFs for netlink reconciliation")
			}
			break
		}
		currentVrfs[resp.Vrf.Name] = resp.Vrf
	}

	// Reconcile each VRF's netlink config
	for _, vrf := range bgpConfig.Spec.Vrfs {
		current := currentVrfs[vrf.Name]
		if current == nil {
			// VRF doesn't exist yet - should be created by reconcileVrfs first
			log.V(1).Info("VRF not found in gobgpd, skipping netlink config", "vrf", vrf.Name)
			continue
		}

		// Handle VRF netlink import
		if vrf.NetlinkImport != nil {
			desiredImportEnabled := vrf.NetlinkImport.Enabled
			currentImportEnabled := current.NetlinkImportEnabled

			if desiredImportEnabled != currentImportEnabled {
				if desiredImportEnabled {
					log.Info("Enabling VRF netlink import",
						"vrf", vrf.Name,
						"interfaces", vrf.NetlinkImport.InterfaceList)

					_, err := apiClient.EnableVrfNetlinkImport(ctx, &gobgpapi.EnableVrfNetlinkImportRequest{
						Vrf:        vrf.Name,
						Interfaces: vrf.NetlinkImport.InterfaceList,
					})
					if err != nil {
						log.Error(err, "Failed to enable VRF netlink import", "vrf", vrf.Name)
						return err
					}
					log.Info("Enabled VRF netlink import", "vrf", vrf.Name)
				} else {
					log.Info("Disabling VRF netlink import", "vrf", vrf.Name)

					_, err := apiClient.DisableVrfNetlinkImport(ctx, &gobgpapi.DisableVrfNetlinkImportRequest{
						Vrf:        vrf.Name,
						KeepRoutes: false, // withdraw imported routes from RIB
					})
					if err != nil {
						log.Error(err, "Failed to disable VRF netlink import", "vrf", vrf.Name)
						return err
					}
					log.Info("Disabled VRF netlink import", "vrf", vrf.Name)
				}
			} else if desiredImportEnabled {
				// Check if interfaces changed
				if !reflect.DeepEqual(vrf.NetlinkImport.InterfaceList, current.NetlinkImportInterfaces) {
					// Re-enable with new interfaces (disable first, then enable)
					log.Info("Updating VRF netlink import interfaces",
						"vrf", vrf.Name,
						"currentInterfaces", current.NetlinkImportInterfaces,
						"desiredInterfaces", vrf.NetlinkImport.InterfaceList)

					_, err := apiClient.DisableVrfNetlinkImport(ctx, &gobgpapi.DisableVrfNetlinkImportRequest{
						Vrf:        vrf.Name,
						KeepRoutes: false,
					})
					if err != nil {
						log.Error(err, "Failed to disable VRF netlink import for update", "vrf", vrf.Name)
						return err
					}

					_, err = apiClient.EnableVrfNetlinkImport(ctx, &gobgpapi.EnableVrfNetlinkImportRequest{
						Vrf:        vrf.Name,
						Interfaces: vrf.NetlinkImport.InterfaceList,
					})
					if err != nil {
						log.Error(err, "Failed to re-enable VRF netlink import", "vrf", vrf.Name)
						return err
					}
					log.Info("Updated VRF netlink import interfaces", "vrf", vrf.Name)
				}
			}
		}

		// Handle VRF netlink export
		// Note: VRF struct doesn't expose NetlinkExportEnabled status currently,
		// so we always attempt to enable if desired, or disable if not.
		// The API calls are idempotent so this is safe.
		if vrf.NetlinkExport != nil && vrf.NetlinkExport.Enabled {
			log.Info("Enabling VRF netlink export",
				"vrf", vrf.Name,
				"linuxVrf", vrf.NetlinkExport.LinuxVrf,
				"linuxTableId", vrf.NetlinkExport.LinuxTableId)

			_, err := apiClient.EnableVrfNetlinkExport(ctx, &gobgpapi.EnableVrfNetlinkExportRequest{
				Vrf:    vrf.Name,
				Config: crdToAPIVrfNetlinkExportConfig(vrf.NetlinkExport),
			})
			if err != nil {
				log.Error(err, "Failed to enable VRF netlink export", "vrf", vrf.Name)
				return err
			}
			log.V(1).Info("VRF netlink export enabled/updated", "vrf", vrf.Name)
		} else if vrf.NetlinkExport != nil && !vrf.NetlinkExport.Enabled {
			log.Info("Disabling VRF netlink export", "vrf", vrf.Name)

			_, err := apiClient.DisableVrfNetlinkExport(ctx, &gobgpapi.DisableVrfNetlinkExportRequest{
				Vrf:        vrf.Name,
				KeepRoutes: false, // flush exported routes from Linux kernel
			})
			if err != nil {
				// Ignore "not enabled" errors when trying to disable
				log.V(1).Info("DisableVrfNetlinkExport returned error (may not be enabled)", "vrf", vrf.Name, "error", err)
			} else {
				log.Info("Disabled VRF netlink export", "vrf", vrf.Name)
			}
		}
	}

	return nil
}

// crdToAPIVrfNetlinkExportConfig converts CRD VrfNetlinkExport to API VrfNetlinkExportConfig
func crdToAPIVrfNetlinkExportConfig(crd *bgpv1.VrfNetlinkExport) *gobgpapi.VrfNetlinkExportConfig {
	if crd == nil {
		return nil
	}
	skipNexthopValidation := false // default to validate
	if crd.ValidateNexthop != nil {
		skipNexthopValidation = !*crd.ValidateNexthop // invert: ValidateNexthop=true means SkipNexthopValidation=false
	}
	return &gobgpapi.VrfNetlinkExportConfig{
		LinuxVrf:              crd.LinuxVrf,
		LinuxTableId:          crd.LinuxTableId,
		Metric:                crd.Metric,
		SkipNexthopValidation: skipNexthopValidation,
		CommunityList:         crd.CommunityList,
		LargeCommunityList:    crd.LargeCommunityList,
	}
}

// crdToAPINetlinkExportRules converts CRD NetlinkExportRule slice to API NetlinkExportRuleConfig slice
func crdToAPINetlinkExportRules(rules []bgpv1.NetlinkExportRule) []*gobgpapi.NetlinkExportRuleConfig {
	if len(rules) == 0 {
		return nil
	}
	apiRules := make([]*gobgpapi.NetlinkExportRuleConfig, 0, len(rules))
	for _, rule := range rules {
		validateNexthop := true // default to true
		if rule.ValidateNexthop != nil {
			validateNexthop = *rule.ValidateNexthop
		}
		apiRules = append(apiRules, &gobgpapi.NetlinkExportRuleConfig{
			Name:               rule.Name,
			CommunityList:      rule.CommunityList,
			LargeCommunityList: rule.LargeCommunityList,
			Vrf:                rule.Vrf,
			TableId:            rule.TableId,
			Metric:             rule.Metric,
			ValidateNexthop:    validateNexthop,
		})
	}
	return apiRules
}

// netlinkExportRulesMatch compares current gobgpd rules with desired CRD rules
func netlinkExportRulesMatch(current []*gobgpapi.ListNetlinkExportRulesResponse_ExportRule, desired []bgpv1.NetlinkExportRule) bool {
	if len(current) != len(desired) {
		return false
	}

	// Build a map of current rules by name for easier lookup
	currentMap := make(map[string]*gobgpapi.ListNetlinkExportRulesResponse_ExportRule)
	for _, rule := range current {
		currentMap[rule.Name] = rule
	}

	// Compare each desired rule with its current counterpart
	for _, desiredRule := range desired {
		currentRule, exists := currentMap[desiredRule.Name]
		if !exists {
			return false
		}

		// Compare community lists
		if !reflect.DeepEqual(currentRule.CommunityList, desiredRule.CommunityList) {
			return false
		}
		if !reflect.DeepEqual(currentRule.LargeCommunityList, desiredRule.LargeCommunityList) {
			return false
		}

		// Compare other fields
		if currentRule.Vrf != desiredRule.Vrf {
			return false
		}
		if currentRule.TableId != desiredRule.TableId {
			return false
		}
		if currentRule.Metric != desiredRule.Metric {
			return false
		}

		// Compare ValidateNexthop (desired defaults to true if nil)
		desiredValidateNexthop := true
		if desiredRule.ValidateNexthop != nil {
			desiredValidateNexthop = *desiredRule.ValidateNexthop
		}
		if currentRule.ValidateNexthop != desiredValidateNexthop {
			return false
		}
	}

	return true
}

// --- Conversion Helpers ---

func crdToAPIDefinedSet(crd *bgpv1.DefinedSet) *gobgpapi.DefinedSet {
	var prefixes []*gobgpapi.Prefix
	for _, p := range crd.Prefixes {
		prefixes = append(prefixes, &gobgpapi.Prefix{
			IpPrefix:      p.IpPrefix,
			MaskLengthMin: p.MaskLengthMin,
			MaskLengthMax: p.MaskLengthMax,
		})
	}
	return &gobgpapi.DefinedSet{
		DefinedType: crdToAPIDefinedType(crd.Type),
		Name:        crd.Name,
		List:        crd.List,
		Prefixes:    prefixes,
	}
}

func crdToAPIVrf(crd *bgpv1.Vrf) *gobgpapi.Vrf {
	var importRts []*gobgpapi.RouteTarget
	for _, rt := range crd.ImportRt {
		importRts = append(importRts, parseRouteTarget(rt))
	}
	var exportRts []*gobgpapi.RouteTarget
	for _, rt := range crd.ExportRt {
		exportRts = append(exportRts, parseRouteTarget(rt))
	}
	return &gobgpapi.Vrf{
		Name:     crd.Name,
		Rd:       parseRouteDistinguisher(crd.Rd),
		ImportRt: importRts,
		ExportRt: exportRts,
	}
}

// parseRouteDistinguisher parses a route distinguisher string (e.g., "65000:100")
func parseRouteDistinguisher(rd string) *gobgpapi.RouteDistinguisher {
	if rd == "" {
		return nil
	}
	// Simple parsing for type 0 RD (ASN:value)
	parts := strings.Split(rd, ":")
	if len(parts) != 2 {
		return nil
	}
	var admin uint32
	var assigned uint32
	if _, err := fmt.Sscanf(parts[0], "%d", &admin); err != nil {
		return nil
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &assigned); err != nil {
		return nil
	}
	return &gobgpapi.RouteDistinguisher{
		Rd: &gobgpapi.RouteDistinguisher_TwoOctetAsn{
			TwoOctetAsn: &gobgpapi.RouteDistinguisherTwoOctetASN{
				Admin:    admin,
				Assigned: assigned,
			},
		},
	}
}

// parseRouteTarget parses a route target string (e.g., "65000:100")
func parseRouteTarget(rt string) *gobgpapi.RouteTarget {
	if rt == "" {
		return nil
	}
	// Simple parsing for type 0 RT (ASN:value)
	parts := strings.Split(rt, ":")
	if len(parts) != 2 {
		return nil
	}
	var asn uint32
	var localAdmin uint32
	if _, err := fmt.Sscanf(parts[0], "%d", &asn); err != nil {
		return nil
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &localAdmin); err != nil {
		return nil
	}
	return &gobgpapi.RouteTarget{
		Rt: &gobgpapi.RouteTarget_TwoOctetAsSpecific{
			TwoOctetAsSpecific: &gobgpapi.TwoOctetAsSpecificExtended{
				IsTransitive: true,
				SubType:      2, // Route Target subtype
				Asn:          asn,
				LocalAdmin:   localAdmin,
			},
		},
	}
}

func crdToAPIPolicy(crd *bgpv1.PolicyDefinition) *gobgpapi.Policy {
	var statements []*gobgpapi.Statement
	for _, s := range crd.Statements {
		statements = append(statements, crdToAPIStatement(&s))
	}
	return &gobgpapi.Policy{
		Name:       crd.Name,
		Statements: statements,
	}
}

func crdToAPIStatement(crd *bgpv1.Statement) *gobgpapi.Statement {
	return &gobgpapi.Statement{
		Name:       crd.Name,
		Conditions: crdToAPIConditions(&crd.Conditions),
		Actions:    crdToAPIActions(&crd.Actions),
	}
}

func crdToAPIConditions(crd *bgpv1.Conditions) *gobgpapi.Conditions {
	return &gobgpapi.Conditions{
		PrefixSet:    crdToAPIMatchSet(crd.PrefixSet),
		NeighborSet:  crdToAPIMatchSet(crd.NeighborSet),
		AsPathSet:    crdToAPIMatchSet(crd.AsPathSet),
		CommunitySet: crdToAPIMatchSet(crd.CommunitySet),
		RpkiResult:   crdToAPIRpkiValidationResult(crd.RpkiResult),
	}
}

func crdToAPIActions(crd *bgpv1.Actions) *gobgpapi.Actions {
	return &gobgpapi.Actions{
		RouteAction: crdToAPIRouteAction(crd.RouteAction),
		Community:   crdToAPICommunityAction(crd.Community),
		Med:         crdToAPIMedAction(crd.Med),
		AsPrepend:   crdToAPIAsPrependAction(crd.AsPrepend),
		LocalPref:   &gobgpapi.LocalPrefAction{Value: crd.LocalPref},
	}
}

// crdToAPIPeerGroupWithPassword converts a CRD PeerGroup to API PeerGroup with resolved password
func (r *BGPConfigurationReconciler) crdToAPIPeerGroupWithPassword(crd *bgpv1.PeerGroup, authPassword string) *gobgpapi.PeerGroup {
	return &gobgpapi.PeerGroup{
		Conf: &gobgpapi.PeerGroupConf{
			PeerGroupName: crd.Config.PeerGroupName,
			PeerAsn:       crd.Config.PeerAsn,
			LocalAsn:      crd.Config.LocalAsn,
			Description:   crd.Config.Description,
			AuthPassword:  authPassword,
		},
		AfiSafis:    crdToAPIAfiSafis(crd.AfiSafis),
		ApplyPolicy: crdToAPIApplyPolicy(crd.ApplyPolicy),
		Timers:      crdToAPITimers(crd.Timers),
		Transport:   crdToAPITransport(crd.Transport),
	}
}

// crdToAPINeighborWithPassword converts a CRD Neighbor to API Peer with resolved password
func (r *BGPConfigurationReconciler) crdToAPINeighborWithPassword(crd *bgpv1.Neighbor, authPassword string) *gobgpapi.Peer {
	return &gobgpapi.Peer{
		Conf: &gobgpapi.PeerConf{
			NeighborAddress:   crd.Config.NeighborAddress,
			PeerAsn:           crd.Config.PeerAsn,
			LocalAsn:          crd.Config.LocalAsn,
			Description:       crd.Config.Description,
			AuthPassword:      authPassword,
			PeerGroup:         crd.Config.PeerGroup,
			AdminDown:         crd.Config.AdminDown,
			NeighborInterface: crd.Config.NeighborInterface,
			Vrf:               crd.Config.Vrf,
		},
		AfiSafis:        crdToAPIAfiSafis(crd.AfiSafis),
		ApplyPolicy:     crdToAPIApplyPolicy(crd.ApplyPolicy),
		Timers:          crdToAPITimers(crd.Timers),
		Transport:       crdToAPITransport(crd.Transport),
		GracefulRestart: crdToAPIGracefulRestart(crd.GracefulRestart),
		RouteReflector:  crdToAPIRouteReflector(crd.RouteReflector),
		EbgpMultihop:    crdToAPIEbgpMultihop(crd.EbgpMultihop),
	}
}

// --- Secret Resolution Helper ---

// resolveAuthPassword resolves the authentication password from either inline value or Secret reference.
// Returns an error if a SecretRef is specified but the Secret or key cannot be found.
func (r *BGPConfigurationReconciler) resolveAuthPassword(ctx context.Context, namespace string, authPassword string, secretRef *corev1.SecretKeySelector, log logr.Logger) (string, error) {
	// If SecretRef is specified, prefer it over inline password
	if secretRef != nil && secretRef.Name != "" {
		secret := &corev1.Secret{}
		secretName := types.NamespacedName{
			Namespace: namespace,
			Name:      secretRef.Name,
		}
		if err := r.Get(ctx, secretName, secret); err != nil {
			return "", fmt.Errorf("failed to get Secret %q for auth password: %w", secretRef.Name, err)
		}
		if password, ok := secret.Data[secretRef.Key]; ok {
			return string(password), nil
		}
		return "", fmt.Errorf("key %q not found in Secret %q", secretRef.Key, secretRef.Name)
	}
	// Fall back to inline password (deprecated)
	return authPassword, nil
}

// --- Enum and Struct Conversion Helpers ---
func crdToAPIAfiSafis(crds []bgpv1.AfiSafi) []*gobgpapi.AfiSafi {
	var apiAfiSafis []*gobgpapi.AfiSafi
	for _, crd := range crds {
		apiAfiSafis = append(apiAfiSafis, &gobgpapi.AfiSafi{
			Config: &gobgpapi.AfiSafiConfig{
				Family:  crdToAPIFamily(crd.Family),
				Enabled: crd.Enabled,
			},
			// AddPaths, PrefixLimit, etc. would be converted here
		})
	}
	return apiAfiSafis
}

func crdToAPIApplyPolicy(crd *bgpv1.ApplyPolicy) *gobgpapi.ApplyPolicy {
	if crd == nil {
		return nil
	}
	return &gobgpapi.ApplyPolicy{
		ExportPolicy: crdToAPIPolicyAssignment(crd.ExportPolicy),
		ImportPolicy: crdToAPIPolicyAssignment(crd.ImportPolicy),
	}
}

func crdToAPIPolicyAssignment(crd *bgpv1.PolicyAssignment) *gobgpapi.PolicyAssignment {
	if crd == nil {
		return nil
	}
	var policies []*gobgpapi.Policy
	for _, pName := range crd.Policies {
		policies = append(policies, &gobgpapi.Policy{Name: pName})
	}
	return &gobgpapi.PolicyAssignment{
		Name:          crd.Name,
		DefaultAction: crdToAPIRouteAction(crd.DefaultAction),
		Policies:      policies,
	}
}

func crdToAPITimers(crd *bgpv1.Timers) *gobgpapi.Timers {
	if crd == nil {
		return nil
	}
	return &gobgpapi.Timers{
		Config: &gobgpapi.TimersConfig{
			ConnectRetry:                 crd.Config.ConnectRetry,
			HoldTime:                     crd.Config.HoldTime,
			KeepaliveInterval:            crd.Config.KeepaliveInterval,
			MinimumAdvertisementInterval: crd.Config.MinimumAdvertisementInterval,
		},
	}
}

func crdToAPITransport(crd *bgpv1.Transport) *gobgpapi.Transport {
	if crd == nil {
		return nil
	}
	return &gobgpapi.Transport{
		LocalAddress:  crd.LocalAddress,
		PassiveMode:   crd.PassiveMode,
		BindInterface: crd.BindInterface,
	}
}

func crdToAPIGracefulRestart(crd *bgpv1.GracefulRestart) *gobgpapi.GracefulRestart {
	if crd == nil {
		return nil
	}
	return &gobgpapi.GracefulRestart{
		Enabled:     crd.Enabled,
		RestartTime: crd.RestartTime,
		HelperOnly:  crd.HelperOnly,
	}
}

func crdToAPIRouteReflector(crd *bgpv1.RouteReflector) *gobgpapi.RouteReflector {
	if crd == nil {
		return nil
	}
	return &gobgpapi.RouteReflector{
		RouteReflectorClient:    crd.RouteReflectorClient,
		RouteReflectorClusterId: crd.RouteReflectorClusterId,
	}
}

func crdToAPIEbgpMultihop(crd *bgpv1.EbgpMultihop) *gobgpapi.EbgpMultihop {
	if crd == nil {
		return nil
	}
	return &gobgpapi.EbgpMultihop{
		Enabled:     crd.Enabled,
		MultihopTtl: crd.MultihopTtl,
	}
}

func crdToAPIDefinedType(s string) gobgpapi.DefinedType {
	switch strings.ToLower(s) {
	case "prefix":
		return gobgpapi.DefinedType_DEFINED_TYPE_PREFIX
	case "neighbor":
		return gobgpapi.DefinedType_DEFINED_TYPE_NEIGHBOR
	case "as-path":
		return gobgpapi.DefinedType_DEFINED_TYPE_AS_PATH
	case "community":
		return gobgpapi.DefinedType_DEFINED_TYPE_COMMUNITY
	default:
		return gobgpapi.DefinedType_DEFINED_TYPE_PREFIX // Default
	}
}

func crdToAPIMatchSet(crd *bgpv1.MatchSet) *gobgpapi.MatchSet {
	if crd == nil {
		return nil
	}
	return &gobgpapi.MatchSet{
		Name: crd.Name,
		// Type conversion needed
	}
}

func crdToAPIRpkiValidationResult(s string) gobgpapi.ValidationState {
	switch strings.ToLower(s) {
	case "valid":
		return gobgpapi.ValidationState_VALIDATION_STATE_VALID
	case "invalid":
		return gobgpapi.ValidationState_VALIDATION_STATE_INVALID
	case "not-found":
		return gobgpapi.ValidationState_VALIDATION_STATE_NOT_FOUND
	default:
		return gobgpapi.ValidationState_VALIDATION_STATE_NONE
	}
}

func crdToAPIRouteAction(s string) gobgpapi.RouteAction {
	switch strings.ToLower(s) {
	case "accept":
		return gobgpapi.RouteAction_ROUTE_ACTION_ACCEPT
	case "reject":
		return gobgpapi.RouteAction_ROUTE_ACTION_REJECT
	default:
		return gobgpapi.RouteAction_ROUTE_ACTION_UNSPECIFIED
	}
}

func crdToAPICommunityAction(crd *bgpv1.CommunityAction) *gobgpapi.CommunityAction {
	if crd == nil {
		return nil
	}
	return &gobgpapi.CommunityAction{
		Communities: crd.Communities,
		// Type conversion needed
	}
}

func crdToAPIMedAction(crd *bgpv1.MedAction) *gobgpapi.MedAction {
	if crd == nil {
		return nil
	}
	return &gobgpapi.MedAction{
		Value: crd.Value,
		// Type conversion needed
	}
}

func crdToAPIAsPrependAction(crd *bgpv1.AsPrependAction) *gobgpapi.AsPrependAction {
	if crd == nil {
		return nil
	}
	return &gobgpapi.AsPrependAction{
		Asn:    crd.Asn,
		Repeat: crd.Repeat,
	}
}

func crdToAPIFamily(s string) *gobgpapi.Family {
	// This is a simplified mapping. A real implementation would be more robust.
	switch strings.ToLower(s) {
	case "ipv4-unicast":
		return &gobgpapi.Family{Afi: gobgpapi.Family_AFI_IP, Safi: gobgpapi.Family_SAFI_UNICAST}
	case "ipv6-unicast":
		return &gobgpapi.Family{Afi: gobgpapi.Family_AFI_IP6, Safi: gobgpapi.Family_SAFI_UNICAST}
	case "l2vpn-evpn":
		return &gobgpapi.Family{Afi: gobgpapi.Family_AFI_L2VPN, Safi: gobgpapi.Family_SAFI_EVPN}
	default:
		return &gobgpapi.Family{Afi: gobgpapi.Family_AFI_UNSPECIFIED, Safi: gobgpapi.Family_SAFI_UNSPECIFIED}
	}
}

func (r *BGPConfigurationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Initialize node cache for router ID resolution
	r.InitNodeCache()

	return ctrl.NewControllerManagedBy(mgr).
		For(&bgpv1.BGPConfiguration{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 1,
			RateLimiter:             workqueue.NewTypedItemExponentialFailureRateLimiter[ctrl.Request](time.Second, 5*time.Minute),
		}).
		Complete(r)
}
