package controllers

import (
	"context"
	"reflect"
	"strings"

	"github.com/go-logr/logr"
	gobgpk8siov1alpha1 "github.com/adamd/k8gobgp/api/v1alpha1"
	gobgpapi "github.com/osrg/gobgp/v3/api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
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

func (r *BGPConfigurationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("bgpconfiguration", req.NamespacedName)

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

	if err := r.reconcileGlobal(ctx, apiClient, bgpConfig, log); err != nil {
		log.Error(err, "failed to reconcile global config")
		return ctrl.Result{}, err
	}
	if err := r.reconcilePeerGroups(ctx, apiClient, bgpConfig, log); err != nil {
		log.Error(err, "failed to reconcile peer groups")
		return ctrl.Result{}, err
	}
	if err := r.reconcileNeighbors(ctx, apiClient, bgpConfig, log); err != nil {
		log.Error(err, "failed to reconcile neighbors")
		return ctrl.Result{}, err
	}
	if err := r.reconcileDynamicNeighbors(ctx, apiClient, bgpConfig, log); err != nil {
		log.Error(err, "failed to reconcile dynamic neighbors")
		return ctrl.Result{}, err
	}
	if err := r.reconcileVrfs(ctx, apiClient, bgpConfig, log); err != nil {
		log.Error(err, "failed to reconcile VRFs")
		return ctrl.Result{}, err
	}

	bgpConfig.Status.ObservedGeneration = bgpConfig.Generation
	if err := r.Status().Update(ctx, bgpConfig); err != nil {
		if errors.IsNotFound(err) {
			log.Info("BGPConfiguration resource not found. Ignoring since object must be deleted.")
			return ctrl.Result{}, nil
		}
		log.Error(err, "failed to update BGPConfiguration status")
		return ctrl.Result{}, err
	}

	log.Info("Successfully reconciled BGPConfiguration")
	return ctrl.Result{}, nil
}

func (r *BGPConfigurationReconciler) reconcileGlobal(ctx context.Context, apiClient gobgpapi.GobgpApiClient, bgpConfig *gobgpk8siov1alpha1.BGPConfiguration, log logr.Logger) error {
	desired := crdToAPIGlobal(&bgpConfig.Spec.Global)
	current, err := apiClient.GetBgp(ctx, &gobgpapi.GetBgpRequest{})
	if err != nil {
		log.Info("BGP server not running or configured, calling StartBgp")
		_, startErr := apiClient.StartBgp(ctx, &gobgpapi.StartBgpRequest{Global: desired})
		return startErr
	}

	if !reflect.DeepEqual(desired, current.Global) {
		log.Info("Global BGP configuration has changed, stopping and restarting BGP server")
		if _, stopErr := apiClient.StopBgp(ctx, &gobgpapi.StopBgpRequest{}); stopErr != nil {
			return stopErr
		}
		if _, startErr := apiClient.StartBgp(ctx, &gobgpapi.StartBgpRequest{Global: desired}); startErr != nil {
			return startErr
		}
	}
	return nil
}

func (r *BGPConfigurationReconciler) reconcilePeerGroups(ctx context.Context, apiClient gobgpapi.GobgpApiClient, bgpConfig *gobgpk8siov1alpha1.BGPConfiguration, log logr.Logger) error {
	// 1. Get current peer groups
	currentPeerGroups := make(map[string]*gobgpapi.PeerGroup)
	stream, err := apiClient.ListPeerGroup(ctx, &gobgpapi.ListPeerGroupRequest{})
	if err != nil {
		return err
	}
	for {
		res, err := stream.Recv()
		if err != nil {
			break // End of stream
		}
		currentPeerGroups[res.PeerGroup.Conf.PeerGroupName] = res.PeerGroup
	}

	// 2. Get desired peer groups
	desiredPeerGroups := make(map[string]*gobgpapi.PeerGroup)
	for _, pg := range bgpConfig.Spec.PeerGroups {
		desiredPeerGroups[pg.Config.PeerGroupName] = crdToAPIPeerGroup(&pg)
	}

	// 3. Delete groups that are no longer desired
	for name := range currentPeerGroups {
		if _, ok := desiredPeerGroups[name]; !ok {
			log.Info("Deleting peer group not in desired state", "name", name)
			if _, err := apiClient.DeletePeerGroup(ctx, &gobgpapi.DeletePeerGroupRequest{Name: name}); err != nil {
				log.Error(err, "Failed to delete peer group", "name", name)
			}
		}
	}

	// 4. Add or Update groups
	for name, desired := range desiredPeerGroups {
		if current, ok := currentPeerGroups[name]; !ok {
			log.Info("Adding new peer group", "name", name)
			if _, err := apiClient.AddPeerGroup(ctx, &gobgpapi.AddPeerGroupRequest{PeerGroup: desired}); err != nil {
				log.Error(err, "Failed to add peer group", "name", name)
			}
		} else {
			if !reflect.DeepEqual(desired.Conf, current.Conf) {
				log.Info("Updating existing peer group", "name", name)
				if _, err := apiClient.UpdatePeerGroup(ctx, &gobgpapi.UpdatePeerGroupRequest{PeerGroup: desired}); err != nil {
					log.Error(err, "Failed to update peer group", "name", name)
				}
			}
		}
	}
	return nil
}

func (r *BGPConfigurationReconciler) reconcileNeighbors(ctx context.Context, apiClient gobgpapi.GobgpApiClient, bgpConfig *gobgpk8siov1alpha1.BGPConfiguration, log logr.Logger) error {
	// 1. Get current peers
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

	// 2. Get desired neighbors
	desiredPeers := make(map[string]*gobgpapi.Peer)
	for _, n := range bgpConfig.Spec.Neighbors {
		desiredPeers[n.Config.NeighborAddress] = crdToAPINeighbor(&n)
	}

	// 3. Delete peers that are no longer desired
	for addr := range currentPeers {
		if _, ok := desiredPeers[addr]; !ok {
			log.Info("Deleting peer not in desired state", "address", addr)
			if _, err := apiClient.DeletePeer(ctx, &gobgpapi.DeletePeerRequest{Address: addr}); err != nil {
				log.Error(err, "Failed to delete peer", "address", addr)
			}
		}
	}

	// 4. Add or Update peers
	for addr, desired := range desiredPeers {
		if current, ok := currentPeers[addr]; !ok {
			log.Info("Adding new peer", "address", addr)
			if _, err := apiClient.AddPeer(ctx, &gobgpapi.AddPeerRequest{Peer: desired}); err != nil {
				log.Error(err, "Failed to add peer", "address", addr)
			}
		} else {
			if !reflect.DeepEqual(desired.Conf, current.Conf) {
				log.Info("Updating existing peer", "address", addr)
				if _, err := apiClient.UpdatePeer(ctx, &gobgpapi.UpdatePeerRequest{Peer: desired}); err != nil {
					log.Error(err, "Failed to update peer", "address", addr)
				}
			}
		}
	}
	return nil
}

func (r *BGPConfigurationReconciler) reconcileDynamicNeighbors(ctx context.Context, apiClient gobgpapi.GobgpApiClient, bgpConfig *gobgpk8siov1alpha1.BGPConfiguration, log logr.Logger) error {
	// 1. Get current dynamic neighbors
	currentDynamicNeighbors := make(map[string]*gobgpapi.DynamicNeighbor)
	stream, err := apiClient.ListDynamicNeighbor(ctx, &gobgpapi.ListDynamicNeighborRequest{})
	if err != nil {
		return err
	}
	for {
		res, err := stream.Recv()
		if err != nil {
			break // End of stream
		}
		currentDynamicNeighbors[res.DynamicNeighbor.Prefix] = res.DynamicNeighbor
	}

	// 2. Get desired dynamic neighbors
	desiredDynamicNeighbors := make(map[string]*gobgpapi.DynamicNeighbor)
	for _, dn := range bgpConfig.Spec.DynamicNeighbors {
		desiredDynamicNeighbors[dn.Prefix] = &gobgpapi.DynamicNeighbor{Prefix: dn.Prefix, PeerGroup: dn.PeerGroup}
	}

	// 3. Delete dynamic neighbors that are no longer desired
	for prefix := range currentDynamicNeighbors {
		if _, ok := desiredDynamicNeighbors[prefix]; !ok {
			log.Info("Deleting dynamic neighbor not in desired state", "prefix", prefix)
			if _, err := apiClient.DeleteDynamicNeighbor(ctx, &gobgpapi.DeleteDynamicNeighborRequest{Prefix: prefix}); err != nil {
				log.Error(err, "Failed to delete dynamic neighbor", "prefix", prefix)
			}
		}
	}

	// 4. Add new dynamic neighbors (Update is not supported, must be deleted and re-added)
	for prefix, desired := range desiredDynamicNeighbors {
		if current, ok := currentDynamicNeighbors[prefix]; !ok {
			log.Info("Adding new dynamic neighbor", "prefix", prefix)
			if _, err := apiClient.AddDynamicNeighbor(ctx, &gobgpapi.AddDynamicNeighborRequest{DynamicNeighbor: desired}); err != nil {
				log.Error(err, "Failed to add dynamic neighbor", "prefix", prefix)
			}
		} else {
			if !reflect.DeepEqual(desired, current) {
				log.Info("Dynamic neighbor has changed, deleting and re-adding", "prefix", prefix)
				if _, err := apiClient.DeleteDynamicNeighbor(ctx, &gobgpapi.DeleteDynamicNeighborRequest{Prefix: prefix}); err != nil {
					log.Error(err, "Failed to delete dynamic neighbor for update", "prefix", prefix)
					continue
				}
				if _, err := apiClient.AddDynamicNeighbor(ctx, &gobgpapi.AddDynamicNeighborRequest{DynamicNeighbor: desired}); err != nil {
					log.Error(err, "Failed to re-add dynamic neighbor for update", "prefix", prefix)
				}
			}
		}
	}
	return nil
}

func (r *BGPConfigurationReconciler) reconcileVrfs(ctx context.Context, apiClient gobgpapi.GobgpApiClient, bgpConfig *gobgpk8siov1alpha1.BGPConfiguration, log logr.Logger) error {
	// 1. Get current VRFs
	currentVrfs := make(map[string]*gobgpapi.Vrf)
	stream, err := apiClient.ListVrf(ctx, &gobgpapi.ListVrfRequest{})
	if err != nil {
		return err
	}
	for {
		res, err := stream.Recv()
		if err != nil {
			break // End of stream
		}
		currentVrfs[res.Vrf.Name] = res.Vrf
	}

	// 2. Get desired VRFs
	desiredVrfs := make(map[string]*gobgpapi.Vrf)
	for _, v := range bgpConfig.Spec.Vrfs {
		// NOTE: RD and RT parsing is complex and requires helpers not included here.
		// This implementation only handles Name and ID.
		desiredVrfs[v.Name] = &gobgpapi.Vrf{Name: v.Name, Id: v.Id}
	}

	// 3. Delete VRFs that are no longer desired
	for name := range currentVrfs {
		if _, ok := desiredVrfs[name]; !ok {
			log.Info("Deleting VRF not in desired state", "name", name)
			if _, err := apiClient.DeleteVrf(ctx, &gobgpapi.DeleteVrfRequest{Name: name}); err != nil {
				log.Error(err, "Failed to delete VRF", "name", name)
			}
		}
	}

	// 4. Add new VRFs (Update is not supported)
	for name, desired := range desiredVrfs {
		if _, ok := currentVrfs[name]; !ok {
			log.Info("Adding new VRF", "name", name)
			if _, err := apiClient.AddVrf(ctx, &gobgpapi.AddVrfRequest{Vrf: desired}); err != nil {
				log.Error(err, "Failed to add VRF", "name", name)
			}
		}
		// Note: No updates. If a VRF's properties change, it must be deleted and recreated manually.
	}
	return nil
}

// --- Conversion Helpers ---

func crdToAPIGlobal(crd *gobgpk8siov1alpha1.GlobalSpec) *gobgpapi.Global {
	return &gobgpapi.Global{
		Asn:             crd.ASN,
		RouterId:        crd.RouterID,
		ListenPort:      crd.ListenPort,
		ListenAddresses: crd.ListenAddresses,
	}
}

func crdToAPIPeerGroup(crd *gobgpk8siov1alpha1.PeerGroup) *gobgpapi.PeerGroup {
	return &gobgpapi.PeerGroup{
		Conf: &gobgpapi.PeerGroupConf{
			PeerGroupName: crd.Config.PeerGroupName,
			PeerAsn:       crd.Config.PeerAsn,
			LocalAsn:      crd.Config.LocalAsn,
			Description:   crd.Config.Description,
			AuthPassword:  crd.Config.AuthPassword,
			// TODO: Add other fields
		},
	}
}

func crdToAPINeighbor(crd *gobgpk8siov1alpha1.Neighbor) *gobgpapi.Peer {
	return &gobgpapi.Peer{
		Conf: &gobgpapi.PeerConf{
			NeighborAddress:   crd.Config.NeighborAddress,
			PeerAsn:           crd.Config.PeerAsn,
			LocalAsn:          crd.Config.LocalAsn,
			Description:       crd.Config.Description,
			AuthPassword:      crd.Config.AuthPassword,
			PeerGroup:         crd.Config.PeerGroup,
			AdminDown:         crd.Config.AdminDown,
			NeighborInterface: crd.Config.NeighborInterface,
			Vrf:               crd.Config.Vrf,
			AllowOwnAsn:       crd.Config.AllowOwnAsn,
			ReplacePeerAsn:    crd.Config.ReplacePeerAsn,
			RemovePrivate:     crdToAPIRemovePrivateType(crd.Config.RemovePrivate),
		},
	}
}

func crdToAPIRemovePrivateType(s string) gobgpapi.RemovePrivate {
	switch strings.ToUpper(s) {
	case "ALL":
		return gobgpapi.RemovePrivate_REMOVE_ALL
	case "REPLACE":
		return gobgpapi.RemovePrivate_REPLACE
	default:
		return gobgpapi.RemovePrivate_REMOVE_NONE
	}
}

func (r *BGPConfigurationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gobgpk8siov1alpha1.BGPConfiguration{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		Complete(r)
}
