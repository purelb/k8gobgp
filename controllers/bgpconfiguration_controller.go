package controllers

import (
	"context"
	"fmt"
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
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	bgpv1 "github.com/adamd/k8gobgp/api/v1"
	gobgpapi "github.com/osrg/gobgp/v4/api"
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
}

// +kubebuilder:rbac:groups=bgp.purelb.io,resources=configs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=bgp.purelb.io,resources=configs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=bgp.purelb.io,resources=configs/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

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
		cleanupSucceeded := false
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
		cleanupSucceeded = true

		if cleanupSucceeded {
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
	}

	return ctrl.Result{}, nil
}

// updateStatusCondition updates a condition on the BGPConfiguration status
func (r *BGPConfigurationReconciler) updateStatusCondition(ctx context.Context, bgpConfig *bgpv1.BGPConfiguration, condType string, status metav1.ConditionStatus, reason, message string) {
	condition := metav1.Condition{
		Type:               condType,
		Status:             status,
		ObservedGeneration: bgpConfig.Generation,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	}

	// Find and update existing condition or append new one
	found := false
	for i, c := range bgpConfig.Status.Conditions {
		if c.Type == condType {
			if c.Status != status {
				bgpConfig.Status.Conditions[i] = condition
			}
			found = true
			break
		}
	}
	if !found {
		bgpConfig.Status.Conditions = append(bgpConfig.Status.Conditions, condition)
	}
}

func (r *BGPConfigurationReconciler) reconcileGlobal(ctx context.Context, apiClient gobgpapi.GoBgpServiceClient, bgpConfig *bgpv1.BGPConfiguration, log logr.Logger) error {
	desired := crdToAPIGlobal(&bgpConfig.Spec.Global)
	current, err := apiClient.GetBgp(ctx, &gobgpapi.GetBgpRequest{})
	if err != nil {
		// gRPC error - server may not be accessible
		log.Info("BGP server not running, calling StartBgp")
		_, startErr := apiClient.StartBgp(ctx, &gobgpapi.StartBgpRequest{Global: desired})
		return startErr
	}

	// GetBgp returns empty config (ASN=0) when gobgpd started but StartBgp never called
	if current.Global.Asn == 0 {
		log.Info("BGP server not initialized, calling StartBgp")
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
			break
		}
		currentPeerGroups[res.PeerGroup.Conf.PeerGroupName] = res.PeerGroup
	}

	// 2. Get desired peer groups (with password resolution from Secrets)
	desiredPeerGroups := make(map[string]*gobgpapi.PeerGroup)
	for _, pg := range bgpConfig.Spec.PeerGroups {
		// Resolve auth password from Secret or inline value
		authPassword := r.resolveAuthPassword(ctx, bgpConfig.Namespace, pg.Config.AuthPassword, pg.Config.AuthPasswordSecretRef, log)
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
			break
		}
		currentNeighbors[res.Peer.Conf.NeighborAddress] = res.Peer
	}

	// 2. Get desired neighbors (with password resolution from Secrets)
	desiredNeighbors := make(map[string]*gobgpapi.Peer)
	for _, n := range bgpConfig.Spec.Neighbors {
		// Resolve auth password from Secret or inline value
		authPassword := r.resolveAuthPassword(ctx, bgpConfig.Namespace, n.Config.AuthPassword, n.Config.AuthPasswordSecretRef, log)
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

// --- Conversion Helpers ---

func crdToAPIGlobal(crd *bgpv1.GlobalSpec) *gobgpapi.Global {
	return &gobgpapi.Global{
		Asn:             crd.ASN,
		RouterId:        crd.RouterID,
		ListenPort:      crd.ListenPort,
		ListenAddresses: crd.ListenAddresses,
	}
}

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

// resolveAuthPassword resolves the authentication password from either inline value or Secret reference
func (r *BGPConfigurationReconciler) resolveAuthPassword(ctx context.Context, namespace string, authPassword string, secretRef *corev1.SecretKeySelector, log logr.Logger) string {
	// If SecretRef is specified, prefer it over inline password
	if secretRef != nil && secretRef.Name != "" {
		secret := &corev1.Secret{}
		secretName := types.NamespacedName{
			Namespace: namespace,
			Name:      secretRef.Name,
		}
		if err := r.Get(ctx, secretName, secret); err != nil {
			log.Error(err, "Failed to get Secret for auth password", "secret", secretRef.Name)
			return ""
		}
		if password, ok := secret.Data[secretRef.Key]; ok {
			return string(password)
		}
		log.Error(nil, "Secret key not found", "secret", secretRef.Name, "key", secretRef.Key)
		return ""
	}
	// Fall back to inline password (deprecated)
	return authPassword
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
	return ctrl.NewControllerManagedBy(mgr).
		For(&bgpv1.BGPConfiguration{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 1,
			RateLimiter:             workqueue.NewTypedItemExponentialFailureRateLimiter[ctrl.Request](time.Second, 5*time.Minute),
		}).
		Complete(r)
}
