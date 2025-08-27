package controllers

import (
	"context"
	"reflect"
	"strings"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	gobgpk8siov1alpha1 "github.com/adamd/k8gobgp/api/v1alpha1"
	gobgpapi "github.com/osrg/gobgp/v3/api"
)

type BGPConfigurationReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=bgp.example.com,resources=bgpconfigurations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=bgp.example.com,resources=bgpconfigurations/status,verbs=get;update;patch

func (r *BGPConfigurationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("bgpconfiguration", req.NamespacedName)
	bgpConfig := &gobgpk8siov1alpha1.BGPConfiguration{}
	if err := r.Get(ctx, req.NamespacedName, bgpConfig); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.Info("Reconciling BGPConfiguration")
	conn, err := grpc.DialContext(ctx, "localhost:50051", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Error(err, "Failed to connect to gobgpd")
		return ctrl.Result{}, err
	}
	defer conn.Close()
	apiClient := gobgpapi.NewGobgpApiClient(conn)

	// Order of reconciliation is important: Global -> Sets -> Policies -> Groups -> Neighbors
	if err := r.reconcileGlobal(ctx, apiClient, bgpConfig, log); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileDefinedSets(ctx, apiClient, bgpConfig, log); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcilePolicies(ctx, apiClient, bgpConfig, log); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcilePeerGroups(ctx, apiClient, bgpConfig, log); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileNeighbors(ctx, apiClient, bgpConfig, log); err != nil {
		return ctrl.Result{}, err
	}

	bgpConfig.Status.ObservedGeneration = bgpConfig.Generation
	if err := r.Status().Update(ctx, bgpConfig); err != nil {
		log.Error(err, "Failed to update BGPConfiguration status")
		return ctrl.Result{}, err
	}

	log.Info("Successfully reconciled BGPConfiguration")
	return ctrl.Result{}, nil
}

func (r *BGPConfigurationReconciler) reconcileGlobal(ctx context.Context, apiClient gobgpapi.GobgpApiClient, bgpConfig *gobgpk8siov1alpha1.BGPConfiguration, log logr.Logger) error {
	desired := crdToAPIGlobal(&bgpConfig.Spec.Global)
	current, err := apiClient.GetBgp(ctx, &gobgpapi.GetBgpRequest{})
	if err != nil {
		log.Info("BGP server not running, calling StartBgp")
		_, startErr := apiClient.StartBgp(ctx, &gobgpapi.StartBgpRequest{Global: desired})
		return startErr
	}
	if !reflect.DeepEqual(desired, current.Global) {
		log.Info("Global config changed, restarting BGP server")
		_, stopErr := apiClient.StopBgp(ctx, &gobgpapi.StopBgpRequest{})
		if stopErr != nil {
			return stopErr
		}
		_, startErr := apiClient.StartBgp(ctx, &gobgpapi.StartBgpRequest{Global: desired})
		return startErr
	}
	return nil
}

func (r *BGPConfigurationReconciler) reconcileDefinedSets(ctx context.Context, apiClient gobgpapi.GobgpApiClient, bgpConfig *gobgpk8siov1alpha1.BGPConfiguration, log logr.Logger) error {
	// This is a simplified reconciliation. A full implementation would compare current and desired sets.
	// For now, we delete all existing sets and re-add them.
	if _, err := apiClient.DeleteDefinedSet(ctx, &gobgpapi.DeleteDefinedSetRequest{All: true}); err != nil {
		log.Error(err, "Failed to delete existing defined sets")
	}
	for _, set := range bgpConfig.Spec.DefinedSets {
		apiSet := crdToAPIDefinedSet(&set)
		if _, err := apiClient.AddDefinedSet(ctx, &gobgpapi.AddDefinedSetRequest{DefinedSet: apiSet}); err != nil {
			log.Error(err, "Failed to add defined set", "name", set.Name)
		}
	}
	return nil
}

func (r *BGPConfigurationReconciler) reconcilePolicies(ctx context.Context, apiClient gobgpapi.GobgpApiClient, bgpConfig *gobgpk8siov1alpha1.BGPConfiguration, log logr.Logger) error {
	// Simplified reconciliation: delete all policies and re-add.
	if _, err := apiClient.DeletePolicy(ctx, &gobgpapi.DeletePolicyRequest{All: true}); err != nil {
		log.Error(err, "Failed to delete existing policies")
	}
	for _, policy := range bgpConfig.Spec.PolicyDefinitions {
		apiPolicy := crdToAPIPolicy(&policy)
		if _, err := apiClient.AddPolicy(ctx, &gobgpapi.AddPolicyRequest{Policy: apiPolicy}); err != nil {
			log.Error(err, "Failed to add policy", "name", policy.Name)
		}
	}
	return nil
}

func (r *BGPConfigurationReconciler) reconcilePeerGroups(ctx context.Context, apiClient gobgpapi.GobgpApiClient, bgpConfig *gobgpk8siov1alpha1.BGPConfiguration, log logr.Logger) error {
	// Simplified reconciliation
	for _, pg := range bgpConfig.Spec.PeerGroups {
		apiPg := crdToAPIPeerGroup(&pg)
		if _, err := apiClient.AddPeerGroup(ctx, &gobgpapi.AddPeerGroupRequest{PeerGroup: apiPg}); err != nil {
			// Attempt an update if add fails (already exists)
			if _, updateErr := apiClient.UpdatePeerGroup(ctx, &gobgpapi.UpdatePeerGroupRequest{PeerGroup: apiPg}); updateErr != nil {
				log.Error(updateErr, "Failed to add or update peer group", "name", pg.Config.PeerGroupName)
			}
		}
	}
	return nil
}

func (r *BGPConfigurationReconciler) reconcileNeighbors(ctx context.Context, apiClient gobgpapi.GobgpApiClient, bgpConfig *gobgpk8siov1alpha1.BGPConfiguration, log logr.Logger) error {
	// Simplified reconciliation
	for _, n := range bgpConfig.Spec.Neighbors {
		apiPeer := crdToAPINeighbor(&n)
		if _, err := apiClient.AddPeer(ctx, &gobgpapi.AddPeerRequest{Peer: apiPeer}); err != nil {
			if _, updateErr := apiClient.UpdatePeer(ctx, &gobgpapi.UpdatePeerRequest{Peer: apiPeer}); updateErr != nil {
				log.Error(updateErr, "Failed to add or update neighbor", "address", n.Config.NeighborAddress)
			}
		}
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

func crdToAPIDefinedSet(crd *gobgpk8siov1alpha1.DefinedSet) *gobgpapi.DefinedSet {
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

func crdToAPIPolicy(crd *gobgpk8siov1alpha1.PolicyDefinition) *gobgpapi.Policy {
	var statements []*gobgpapi.Statement
	for _, s := range crd.Statements {
		statements = append(statements, crdToAPIStatement(&s))
	}
	return &gobgpapi.Policy{
		Name:       crd.Name,
		Statements: statements,
	}
}

func crdToAPIStatement(crd *gobgpk8siov1alpha1.Statement) *gobgpapi.Statement {
	return &gobgpapi.Statement{
		Name:       crd.Name,
		Conditions: crdToAPIConditions(&crd.Conditions),
		Actions:    crdToAPIActions(&crd.Actions),
	}
}

func crdToAPIConditions(crd *gobgpk8siov1alpha1.Conditions) *gobgpapi.Conditions {
	return &gobgpapi.Conditions{
		PrefixSet:    crdToAPIMatchSet(crd.PrefixSet),
		NeighborSet:  crdToAPIMatchSet(crd.NeighborSet),
		AsPathSet:    crdToAPIMatchSet(crd.AsPathSet),
		CommunitySet: crdToAPIMatchSet(crd.CommunitySet),
		RpkiResult:   crdToAPIRpkiValidationResult(crd.RpkiResult),
	}
}

func crdToAPIActions(crd *gobgpk8siov1alpha1.Actions) *gobgpapi.Actions {
	return &gobgpapi.Actions{
		RouteAction: crdToAPIRouteAction(crd.RouteAction),
		Community:   crdToAPICommunityAction(crd.Community),
		Med:         crdToAPIMedAction(crd.Med),
		AsPrepend:   crdToAPIAsPrependAction(crd.AsPrepend),
		LocalPref:   &gobgpapi.LocalPrefAction{Value: crd.LocalPref},
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
		},
		AfiSafis: crdToAPIAfiSafis(crd.AfiSafis),
		ApplyPolicy: crdToAPIApplyPolicy(crd.ApplyPolicy),
		Timers: crdToAPITimers(crd.Timers),
		Transport: crdToAPITransport(crd.Transport),
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

// --- Enum and Struct Conversion Helpers ---
func crdToAPIAfiSafis(crds []gobgpk8siov1alpha1.AfiSafi) []*gobgpapi.AfiSafi {
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

func crdToAPIApplyPolicy(crd *gobgpk8siov1alpha1.ApplyPolicy) *gobgpapi.ApplyPolicy {
	if crd == nil {
		return nil
	}
	return &gobgpapi.ApplyPolicy{
		InPolicy:     crdToAPIPolicyAssignment(crd.InPolicy),
		ExportPolicy: crdToAPIPolicyAssignment(crd.ExportPolicy),
		ImportPolicy: crdToAPIPolicyAssignment(crd.ImportPolicy),
	}
}

func crdToAPIPolicyAssignment(crd *gobgpk8siov1alpha1.PolicyAssignment) *gobgpapi.PolicyAssignment {
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

func crdToAPITimers(crd *gobgpk8siov1alpha1.Timers) *gobgpapi.Timers {
	if crd == nil {
		return nil
	}
	return &gobgpapi.Timers{
		Config: &gobgpapi.TimersConfig{
			ConnectRetry:               crd.Config.ConnectRetry,
			HoldTime:                   crd.Config.HoldTime,
			KeepaliveInterval:          crd.Config.KeepaliveInterval,
			MinimumAdvertisementInterval: crd.Config.MinimumAdvertisementInterval,
		},
	}
}

func crdToAPITransport(crd *gobgpk8siov1alpha1.Transport) *gobgpapi.Transport {
	if crd == nil {
		return nil
	}
	return &gobgpapi.Transport{
		LocalAddress:  crd.LocalAddress,
		PassiveMode:   crd.PassiveMode,
		BindInterface: crd.BindInterface,
	}
}

func crdToAPIGracefulRestart(crd *gobgpk8siov1alpha1.GracefulRestart) *gobgpapi.GracefulRestart {
	if crd == nil {
		return nil
	}
	return &gobgpapi.GracefulRestart{
		Enabled:     crd.Enabled,
		RestartTime: crd.RestartTime,
		HelperOnly:  crd.HelperOnly,
	}
}

func crdToAPIRouteReflector(crd *gobgpk8siov1alpha1.RouteReflector) *gobgpapi.RouteReflector {
	if crd == nil {
		return nil
	}
	return &gobgpapi.RouteReflector{
		RouteReflectorClient:    crd.RouteReflectorClient,
		RouteReflectorClusterId: crd.RouteReflectorClusterId,
	}
}

func crdToAPIEbgpMultihop(crd *gobgpk8siov1alpha1.EbgpMultihop) *gobgpapi.EbgpMultihop {
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
		return gobgpapi.DefinedType_PREFIX
	case "neighbor":
		return gobgpapi.DefinedType_NEIGHBOR
	case "as-path":
		return gobgpapi.DefinedType_AS_PATH
	case "community":
		return gobgpapi.DefinedType_COMMUNITY
	default:
		return gobgpapi.DefinedType_PREFIX // Default
	}
}

func crdToAPIMatchSet(crd *gobgpk8siov1alpha1.MatchSet) *gobgpapi.MatchSet {
	if crd == nil {
		return nil
	}
	return &gobgpapi.MatchSet{
		Name: crd.Name,
		// Type conversion needed
	}
}

func crdToAPIRpkiValidationResult(s string) int32 {
	switch strings.ToLower(s) {
	case "valid":
		return 2
	case "invalid":
		return 3
	case "not-found":
		return 1
	default:
		return 0 // None
	}
}

func crdToAPIRouteAction(s string) gobgpapi.RouteAction {
	switch strings.ToLower(s) {
	case "accept":
		return gobgpapi.RouteAction_ACCEPT
	case "reject":
		return gobgpapi.RouteAction_REJECT
	default:
		return gobgpapi.RouteAction_NONE
	}
}

func crdToAPICommunityAction(crd *gobgpk8siov1alpha1.CommunityAction) *gobgpapi.CommunityAction {
	if crd == nil {
		return nil
	}
	return &gobgpapi.CommunityAction{
		Communities: crd.Communities,
		// Type conversion needed
	}
}

func crdToAPIMedAction(crd *gobgpk8siov1alpha1.MedAction) *gobgpapi.MedAction {
	if crd == nil {
		return nil
	}
	return &gobgpapi.MedAction{
		Value: crd.Value,
		// Type conversion needed
	}
}

func crdToAPIAsPrependAction(crd *gobgpk8siov1alpha1.AsPrependAction) *gobgpapi.AsPrependAction {
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
		return &gobgpapi.Family{Afi: gobgpapi.Family_AFI_UNKNOWN, Safi: gobgpapi.Family_SAFI_UNKNOWN}
	}
}

func (r *BGPConfigurationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gobgpk8siov1alpha1.BGPConfiguration{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		Complete(r)
}