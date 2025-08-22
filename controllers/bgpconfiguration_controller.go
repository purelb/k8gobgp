package controllers

import (
	"context"
	"reflect"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	gobgpk8siov1alpha1 "github.com/adamd/k8gobgp/api/v1alpha1"
	gobgpapi "github.com/osrg/gobgp/v3/api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// BGPConfigurationReconciler reconciles a BGPConfiguration object
type BGPConfigurationReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=gobgp.k8s.io,resources=bgpconfigurations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gobgp.k8s.io,resources=bgpconfigurations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gobgp.k8s.io,resources=bgpconfigurations/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify Reconcile to compare the state specified by
// the BGPConfiguration object against the actual cluster state, and then
// perform operations to make the cluster state reflect the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.8.3/pkg/reconcile
func (r *BGPConfigurationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("bgpconfiguration", req.NamespacedName)

	// Fetch the BGPConfiguration instance
	bgpConfig := &gobgpk8siov1alpha1.BGPConfiguration{}
	if err := r.Get(ctx, req.NamespacedName, bgpConfig); err != nil {
		log.Error(err, "unable to fetch BGPConfiguration")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.Info("Reconciling BGPConfiguration")

	conn, err := grpc.DialContext(ctx, "localhost:50051", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Error(err, "failed to connect to gobgpd")
		return ctrl.Result{}, err
	}
	defer conn.Close()
	apiClient := gobgpapi.NewGobgpApiClient(conn)

	// Construct the desired global config from the spec
	desiredGlobalConfig := &gobgpapi.Global{}
	if bgpConfig.Spec.Global != nil {
		desiredGlobalConfig = &gobgpapi.Global{
			Asn:                   bgpConfig.Spec.Global.ASN,
			RouterId:              bgpConfig.Spec.Global.RouterID,
			ListenPort:            bgpConfig.Spec.Global.ListenPort,
			ListenAddresses:       bgpConfig.Spec.Global.ListenAddresses,
			Families:              bgpConfig.Spec.Global.Families,
			UseMultiplePaths:      bgpConfig.Spec.Global.UseMultiplePaths,
			BindToDevice:          bgpConfig.Spec.Global.BindToDevice,
		}
	}

	// Get the current BGP server state
	currentBgp, err := apiClient.GetBgp(ctx, &gobgpapi.GetBgpRequest{})
	if err != nil {
		// Any error here is treated as the BGP server not being configured yet.
		log.Info("BGP server not running or configured, calling StartBgp")
		_, startErr := apiClient.StartBgp(ctx, &gobgpapi.StartBgpRequest{Global: desiredGlobalConfig})
		if startErr != nil {
			log.Error(startErr, "failed to start BGP server with initial configuration")
			return ctrl.Result{}, startErr
		}
		log.Info("Successfully started BGP server")
	} else {
		// BGP server is running, check if the global config needs updating.
		if !reflect.DeepEqual(desiredGlobalConfig, currentBgp.Global) {
			log.Info("Global BGP configuration has changed, stopping and restarting BGP server to apply changes")

			// Stop the BGP server
			_, stopErr := apiClient.StopBgp(ctx, &gobgpapi.StopBgpRequest{})
			if stopErr != nil {
				log.Error(stopErr, "failed to stop BGP server for reconfiguration")
				return ctrl.Result{}, stopErr
			}

			// Start the BGP server with the new configuration
			_, startErr := apiClient.StartBgp(ctx, &gobgpapi.StartBgpRequest{Global: desiredGlobalConfig})
			if startErr != nil {
				log.Error(startErr, "failed to restart BGP server with updated configuration")
				return ctrl.Result{}, startErr
			}
			log.Info("Successfully restarted BGP server with new configuration")
		} else {
			log.Info("Global BGP configuration is already in the desired state")
		}
	}

	// Reconcile Neighbors
	if err := r.reconcileNeighbors(ctx, apiClient, bgpConfig); err != nil {
		log.Error(err, "failed to reconcile neighbors")
		return ctrl.Result{}, err
	}

	// TODO: Apply other configurations (peer groups, policies, etc.) in a similarly idempotent way.

	// Update status
	bgpConfig.Status.ObservedGeneration = bgpConfig.Generation
	if err := r.Status().Update(ctx, bgpConfig); err != nil {
		if errors.IsNotFound(err) {
			log.Info("BGPConfiguration resource not found. Ignoring since object must be deleted.")
			return ctrl.Result{}, nil
		}
		log.Error(err, "failed to update BGPConfiguration status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *BGPConfigurationReconciler) reconcileNeighbors(ctx context.Context, apiClient gobgpapi.GobgpApiClient, bgpConfig *gobgpk8siov1alpha1.BGPConfiguration) error {
	log := r.Log.WithValues("bgpconfiguration", bgpConfig.Name)

	// 1. Get current peers from gobgpd
	currentPeers := make(map[string]*gobgpapi.Peer)
	stream, err := apiClient.ListPeer(ctx, &gobgpapi.ListPeerRequest{})
	if err != nil {
		return err
	}
	for {
		res, err := stream.Recv()
		if err != nil {
			break // End of stream
		}
		currentPeers[res.Peer.Conf.NeighborAddress] = res.Peer
	}

	// 2. Get desired neighbors from the CRD
	desiredNeighbors := make(map[string]gobgpk8siov1alpha1.Neighbor)
	for _, n := range bgpConfig.Spec.Neighbors {
		desiredNeighbors[n.Config.NeighborAddress] = n
	}

	// 3. Delete peers that are no longer desired
	for addr := range currentPeers {
		if _, ok := desiredNeighbors[addr]; !ok {
			log.Info("Deleting peer not in desired state", "address", addr)
			_, err := apiClient.DeletePeer(ctx, &gobgpapi.DeletePeerRequest{Address: addr})
			if err != nil {
				log.Error(err, "Failed to delete peer", "address", addr)
				// Continue trying to reconcile other peers
			}
		}
	}

	// 4. Add or Update peers
	for addr, desired := range desiredNeighbors {
		apiPeer := &gobgpapi.Peer{
			Conf: &gobgpapi.PeerConf{
				NeighborAddress: desired.Config.NeighborAddress,
				PeerAsn:          desired.Config.PeerAs,
				LocalAsn:         desired.Config.LocalAs,
				AuthPassword:    desired.Config.AuthPassword,
				Description:     desired.Config.Description,
				PeerGroup:       desired.Config.PeerGroup,
				AdminDown:       desired.Config.AdminDown,
			},
			// TODO: Convert other desired fields (Timers, Transport, etc.) to gobgpapi types
		}

		if current, ok := currentPeers[addr]; !ok {
			log.Info("Adding new peer", "address", addr)
			_, err := apiClient.AddPeer(ctx, &gobgpapi.AddPeerRequest{Peer: apiPeer})
			if err != nil {
				log.Error(err, "Failed to add peer", "address", addr)
			}
		} else {
			// Simple comparison for now. A more robust solution would be a deep comparison.
			if !reflect.DeepEqual(apiPeer.Conf, current.Conf) {
				log.Info("Updating existing peer", "address", addr)
				_, err := apiClient.UpdatePeer(ctx, &gobgpapi.UpdatePeerRequest{Peer: apiPeer})
				if err != nil {
					log.Error(err, "Failed to update peer", "address", addr)
				}
			}
		}
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *BGPConfigurationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gobgpk8siov1alpha1.BGPConfiguration{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		Complete(r)
}
