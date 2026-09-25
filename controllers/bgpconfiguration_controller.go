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
	"net/netip"
	"os"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

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

	// NodeStatusReporter is the BGPNodeStatus reporter (set during setup in main.go)
	NodeStatusReporter *BGPNodeStatusReporter
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

// neighborKey returns a stable map key for a GoBGP peer. For interface-based
// (unnumbered) peers, the key is "iface:<name>" since GoBGP resolves the
// link-local address internally and NeighborAddress would differ between
// desired (empty) and current (fe80::...) state.
func neighborKey(conf *gobgpapi.PeerConf) string {
	if conf.NeighborInterface != "" {
		return "iface:" + conf.NeighborInterface
	}
	return conf.NeighborAddress
}

// neighborKeyFromCRD returns a stable map key from CRD neighbor config.
func neighborKeyFromCRD(cfg *bgpv1.NeighborConfig) string {
	if cfg.NeighborInterface != "" {
		return "iface:" + cfg.NeighborInterface
	}
	return cfg.NeighborAddress
}

// routerIDInfoLabels builds the label set for one CR's router_id_info series.
// Every call site must use this so the delete on CR removal matches exactly
// what was set — a mismatch would silently leave the series behind.
func routerIDInfoLabels(bgpConfig *bgpv1.BGPConfiguration, routerID, source, nodeName string) prometheus.Labels {
	return prometheus.Labels{
		"router_id": sanitizeLabelValue(routerID),
		"source":    sanitizeLabelValue(source),
		"node":      sanitizeLabelValue(nodeName),
		"asn":       sanitizeLabelValue(fmt.Sprintf("%d", bgpConfig.Spec.Global.ASN)),
		"name":      sanitizeLabelValue(bgpConfig.Name),
		"namespace": sanitizeLabelValue(bgpConfig.Namespace),
	}
}

// dynamicNeighborPrefixes parses the configured dynamic-neighbor ranges.
// Malformed entries are logged and skipped rather than failing the reconcile:
// CRD validation is the right place to reject them, and one bad prefix must not
// stop the rest of the configuration being applied.
func dynamicNeighborPrefixes(bgpConfig *bgpv1.BGPConfiguration, log logr.Logger) []netip.Prefix {
	if len(bgpConfig.Spec.DynamicNeighbors) == 0 {
		return nil
	}
	prefixes := make([]netip.Prefix, 0, len(bgpConfig.Spec.DynamicNeighbors))
	for _, dn := range bgpConfig.Spec.DynamicNeighbors {
		p, err := netip.ParsePrefix(dn.Prefix)
		if err != nil {
			log.Error(err, "Ignoring malformed dynamicNeighbors prefix", "prefix", dn.Prefix)
			continue
		}
		prefixes = append(prefixes, p)
	}
	return prefixes
}

// isDynamicallyAcceptedPeer reports whether a peer present in gobgpd arrived by
// being accepted from one of the configured dynamic-neighbor ranges, rather
// than from spec.neighbors.
//
// Such peers are never in desiredNeighbors — that map is built solely from
// spec.neighbors — so without this check the reconcile sweep deletes them and
// gobgpd tears the session down with a CEASE notification. Interface-based
// peers are never dynamic, and are excluded by having no parseable address.
func isDynamicallyAcceptedPeer(peer *gobgpapi.Peer, prefixes []netip.Prefix) bool {
	if len(prefixes) == 0 || peer == nil || peer.Conf == nil {
		return false
	}
	addr, err := netip.ParseAddr(peer.Conf.NeighborAddress)
	if err != nil {
		return false
	}
	// Compare unzoned: a zone identifier is meaningless for range containment.
	addr = addr.WithZone("")
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// deletePeerByKey issues a DeletePeer request using either Interface or Address
// depending on whether the peer is interface-based.
func deletePeerByKey(ctx context.Context, apiClient gobgpapi.GoBgpServiceClient, cfg *bgpv1.NeighborConfig) error {
	if cfg.NeighborInterface != "" {
		_, err := apiClient.DeletePeer(ctx, &gobgpapi.DeletePeerRequest{Interface: cfg.NeighborInterface})
		return err
	}
	_, err := apiClient.DeletePeer(ctx, &gobgpapi.DeletePeerRequest{Address: cfg.NeighborAddress})
	return err
}

// peerConfigEqual compares only the controller-managed fields of two GoBGP Peers,
// ignoring runtime state fields that GoBGP populates (State, Timers.State,
// AfiSafi.State, resolved NeighborAddress for interface peers, auto-populated
// Type/SendCommunity). This avoids spurious UpdatePeer calls every reconcile cycle.
func peerConfigEqual(desired, current *gobgpapi.Peer) bool {
	if desired.Conf == nil || current.Conf == nil {
		return desired.Conf == current.Conf
	}

	dc, cc := desired.Conf, current.Conf

	// For interface-based peers, skip NeighborAddress comparison since GoBGP
	// resolves the link-local address and populates it in the current peer.
	if dc.NeighborInterface == "" && dc.NeighborAddress != cc.NeighborAddress {
		return false
	}
	// AuthPassword's value is deliberately not compared. ListPeer redacts it to a
	// non-nil empty string before the peer leaves the server, so the desired value
	// can never match what comes back and asserting on it means an UpdatePeer
	// every reconcile for every authenticated peer.
	//
	// Its *presence* is compared instead, which is free: PeerState.AuthPasswordSet
	// is a plain bool that round-trips, so it cannot churn, and it catches the case
	// that matters - a password the CR asks for that the daemon is not using.
	//
	// The Secret is NOT watched. SetupWithManager registers BGPConfiguration and
	// corev1.Node only, and resolveAuthPassword reads Secrets on demand, so a
	// password-only edit to a Secret does not re-enter Reconcile at all. An earlier
	// version of this comment claimed otherwise.
	//
	// TODO: watch the referenced Secrets, or track the resolved password's hash, so
	// a password-only change is detected without comparing the secret itself.
	//
	// The presence check lives in reconcileNeighbors, not here: AuthPasswordSet is
	// on PeerState, and this function's contract is to ignore runtime state.
	// LocalAsn is compared only when the CR sets one. gobgpd defaults an unset
	// per-peer local AS to the global ASN and echoes the defaulted value, so an
	// unconditional comparison is 0 against the global ASN - permanently
	// unequal for every neighbor that does not override its local AS, which is
	// the overwhelmingly common case. Same guard, and same known gap, as
	// transportConfigEqual's LocalAddress: clearing localAsn to return to the
	// global default is not detected as a change.
	if dc.GetLocalAsn() != 0 && dc.GetLocalAsn() != cc.GetLocalAsn() {
		return false
	}
	// peer-as is in oc's forcedOverwrittenConfig, so a peer group owns it
	// wherever the group states one, and UpdatePeer cannot change that. A
	// grouped member whose CR names a different ASN would read the group's back
	// for ever, so comparing it churns on something we cannot fix. Where the
	// group states none, gobgp-netlink v1.3.5 preserves the member's value, so
	// there is nothing to detect either. Ungrouped peers are compared normally.
	//
	// A difference here for a grouped peer means the group overrode the member,
	// which reconcileNeighbors reports as an event - see reportPeerAsnOverridden.
	if dc.PeerGroup == "" && dc.PeerAsn != cc.PeerAsn {
		return false
	}
	// Description is compared only when the CR states one. The converter sends
	// nil for an omitted description so the peer group can still supply it, and
	// gobgpd echoes the resolved value - comparing that against nil would be
	// permanently unequal for every grouped peer without its own.
	//
	// Presence, not emptiness: description is a *string in the CRD, so
	// `description: ""` is a deliberate override of a group's value and must be
	// compared. A GetDescription() != "" test would silently skip exactly that.
	if dc.Description != nil && dc.GetDescription() != cc.GetDescription() {
		return false
	}
	if dc.PeerGroup != cc.PeerGroup ||
		dc.AdminDown != cc.AdminDown ||
		dc.NeighborInterface != cc.NeighborInterface ||
		dc.Vrf != cc.Vrf {
		return false
	}
	// Explicit-presence fields, measured round-trippable against a live
	// gobgp-netlink v1.3.5: ListPeer reports each one unconditionally
	// (NewPeerFromConfigStruct wraps them in proto.X, "never nil on the read
	// path"), so current is never nil and the getters are always safe.
	//
	// Each is compared only when the CR stated it. The converter sends nil
	// otherwise, which is what lets a peer group supply the value - comparing a
	// nil desired against the group's resolved value would churn for ever.
	// "Stated it" is a non-zero test, because these CRD fields are plain values
	// with omitempty and cannot distinguish unset from explicitly false.
	if dc.AllowOwnAsn != nil && dc.GetAllowOwnAsn() != cc.GetAllowOwnAsn() {
		return false
	}
	if dc.ReplacePeerAsn != nil && dc.GetReplacePeerAsn() != cc.GetReplacePeerAsn() {
		return false
	}
	if dc.AllowAspathLoopLocal != nil && dc.GetAllowAspathLoopLocal() != cc.GetAllowAspathLoopLocal() {
		return false
	}
	if dc.SendSoftwareVersion != nil && dc.GetSendSoftwareVersion() != cc.GetSendSoftwareVersion() {
		return false
	}
	// RemovePrivate rounds trips as of gobgp-netlink v1.3.5: util.go's
	// removePrivateToAPI reports it on both the peer and the peer-group read
	// path. It was previously excluded on the peer side by a comment copied from
	// peerGroupConfigEqual that was never true here.
	if dc.RemovePrivate != nil && dc.GetRemovePrivate() != cc.GetRemovePrivate() {
		return false
	}
	// Compared as of v1.3.1, where it round-trips. In v1.3.0 ListPeer returned
	// 0 for it whatever was sent, so comparing it would have churned forever.
	// The struct field, NOT GetSendCommunity(): for a field with explicit
	// presence the generated getter dereferences and returns 0 when absent,
	// which is "standard" - collapsing exactly the distinction this field
	// gained presence in order to express.
	if !sendCommunityEqual(dc.SendCommunity, cc.SendCommunity) {
		return false
	}

	if !afiSafisConfigEqual(desired.AfiSafis, current.AfiSafis) {
		return false
	}
	// Blocks a peer group can supply are skipped when this neighbor is in a group
	// and its CR did not state the block.
	//
	// Written out per block rather than through a helper taking `any`: a typed nil
	// pointer boxed into an interface is NOT == nil, so a
	// `func(block any) bool { return block == nil }` version compiled, read
	// correctly, and never skipped anything. It went unnoticed because the only
	// block whose comparator does not already tolerate a nil desired is bfd.
	//
	// Presence is block-level on the gRPC path: our converters send nil for an
	// omitted block, which is what lets the group's value apply, and ListPeer then
	// reports the group's resolved value. Comparing that against nil would fail
	// every reconcile and issue an UpdatePeer that cannot change it, because the
	// value belongs to the group.
	//
	// timersConfigEqual needs no such guard - it already tests each field on
	// != 0, so an absent block compares equal - and afiSafisConfigEqual returns
	// true for an empty desired list.
	//
	// Every block a peer group can state is in the skip: afiSafis, applyPolicy,
	// timers, transport, gracefulRestart and bfd. Two earlier versions of this
	// comment named a shorter list from a stale reading of the PeerGroup CRD, and
	// each omission left grouped members churning on a block they had inherited.
	// Re-derive it from api/v1/types.go rather than trusting this sentence.
	grouped := dc.GetPeerGroup() != ""
	if !(grouped && desired.ApplyPolicy == nil) && !applyPolicyEqual(desired.ApplyPolicy, current.ApplyPolicy) {
		return false
	}
	if !timersConfigEqual(desired.Timers, current.Timers) {
		return false
	}
	if !(grouped && desired.Transport == nil) && !transportConfigEqual(desired.Transport, current.Transport) {
		return false
	}
	if !(grouped && desired.GracefulRestart == nil) &&
		!gracefulRestartEqual(desired.GracefulRestart, current.GracefulRestart) {
		return false
	}
	if !(grouped && desired.RouteReflector == nil) && !routeReflectorEqual(desired.RouteReflector, current.RouteReflector) {
		return false
	}
	if !(grouped && desired.EbgpMultihop == nil) && !ebgpMultihopEqual(desired.EbgpMultihop, current.EbgpMultihop) {
		return false
	}
	// defaulted: addNeighbor runs SetDefaultNeighborConfigValues, so ListPeer
	// echoes a fully-populated BFD block for every peer - including peers that
	// have no BFD at all, which is the case that churns if this is skipped.
	//
	// inherits() as well, because the PeerGroup CRD has a bfd block too: a member
	// that omits bfd under a group that sets it reads the group's back, and
	// comparing that against nil fails on every reconcile. Caught by
	// TestRoundTripPeerGroupInheritance/group_sets_bfd,_member_omits_it.
	if !(grouped && desired.Bfd == nil) && !bfdEqual(desired.Bfd, current.Bfd, true) {
		return false
	}
	return true
}

// peerGroupConfigEqual compares only the controller-managed fields of two GoBGP PeerGroups,
// ignoring runtime state (Info field).
func peerGroupConfigEqual(desired, current *gobgpapi.PeerGroup) bool {
	if desired.Conf == nil || current.Conf == nil {
		return desired.Conf == current.Conf
	}

	dc, cc := desired.Conf, current.Conf
	// AuthPassword is not compared, for the same reason as in peerConfigEqual:
	// ListPeerGroup redacts it before the group leaves the server.
	// LocalAsn is compared only when the CR sets one, for the same reason as in
	// peerConfigEqual: gobgpd defaults an unset local AS to the global ASN and
	// echoes the defaulted value. Measured against a live gobgp-netlink v1.3.5 by
	// TestRoundTripPeerGroup/minimal - a group sending only peerAsn and
	// peerGroupName comes back with localAsn set to global.asn, so an
	// unconditional comparison fired UpdatePeerGroup on every reconcile, and
	// updatePeerGroup loops updateNeighbor over every member of the group.
	if dc.LocalAsn != 0 && dc.LocalAsn != cc.LocalAsn {
		return false
	}
	if dc.PeerGroupName != cc.PeerGroupName ||
		dc.PeerAsn != cc.PeerAsn ||
		dc.Description != cc.Description ||
		dc.AllowOwnAsn != cc.AllowOwnAsn ||
		dc.ReplacePeerAsn != cc.ReplacePeerAsn ||
		dc.AllowAspathLoopLocal != cc.AllowAspathLoopLocal ||
		dc.SendSoftwareVersion != cc.SendSoftwareVersion {
		return false
	}
	// RemovePrivate round-trips as of gobgp-netlink v1.3.5: util.go's
	// removePrivateToAPI populates it in NewPeerGroupFromConfigStruct, so
	// ListPeerGroup reports it. Before that it was write-only for peer groups and
	// comparing it meant UpdatePeerGroup every reconcile, which loops
	// updateNeighbor over every member. PeerGroupConf carries plain values, not
	// explicit presence - a peer group is only ever a source of values and never
	// inherits - so this is a direct comparison.
	if dc.RemovePrivate != cc.RemovePrivate {
		return false
	}
	// Compared as of v1.3.1, where it round-trips. In v1.3.0 ListPeer returned
	// 0 for it whatever was sent, so comparing it would have churned forever.
	// The struct field, NOT GetSendCommunity(): for a field with explicit
	// presence the generated getter dereferences and returns 0 when absent,
	// which is "standard" - collapsing exactly the distinction this field
	// gained presence in order to express.
	if !sendCommunityEqual(dc.SendCommunity, cc.SendCommunity) {
		return false
	}

	if !afiSafisConfigEqual(desired.AfiSafis, current.AfiSafis) {
		return false
	}
	if !applyPolicyEqual(desired.ApplyPolicy, current.ApplyPolicy) {
		return false
	}
	if !timersConfigEqual(desired.Timers, current.Timers) {
		return false
	}
	if !transportConfigEqual(desired.Transport, current.Transport) {
		return false
	}
	if !gracefulRestartEqual(desired.GracefulRestart, current.GracefulRestart) {
		return false
	}
	// Not defaulted, unlike the peer case above: addPeerGroup calls no
	// defaulting function at all, so ListPeerGroup echoes the block as sent.
	// Defaulting here would make every peer group permanently unequal, and
	// UpdatePeerGroup loops updateNeighbor over every member.
	if !bfdEqual(desired.Bfd, current.Bfd, false) {
		return false
	}
	return true
}

// afiSafisConfigEqual compares AFI/SAFI config only (ignoring State).
// afiSafisConfigEqual compares the afi-safi list, and only when the CR sets one.
//
// A neighbor that omits afiSafis sends nil, and gobgpd answers with its default
// family set - populated entries for whatever global.families says, or
// ipv4-unicast when it says nothing. Comparing lengths then gives 0 against 1+
// and UpdatePeer fires on every reconcile for every neighbor that does not spell
// its families out.
//
// Found by TestRoundTripNeighbor against a real daemon. The cluster used to
// check for churn happened to set afiSafis on every neighbor, so it never
// exercised this - which is exactly the gap a round-trip harness closes and a
// single production config cannot.
//
// Same guard, and the same known gap, as the other "compare only what the CR
// sets" cases: removing the afiSafis block to fall back to the default set is
// not detected as a change.
func afiSafisConfigEqual(desired, current []*gobgpapi.AfiSafi) bool {
	if len(desired) == 0 {
		return true
	}
	if len(desired) != len(current) {
		return false
	}
	for i := range desired {
		if desired[i].Config == nil || current[i].Config == nil {
			if desired[i].Config != current[i].Config {
				return false
			}
			continue
		}
		if desired[i].Config.Family == nil || current[i].Config.Family == nil {
			if desired[i].Config.Family != current[i].Config.Family {
				return false
			}
			continue
		}
		if desired[i].Config.Family.Afi != current[i].Config.Family.Afi ||
			desired[i].Config.Family.Safi != current[i].Config.Family.Safi ||
			desired[i].Config.Enabled != current[i].Config.Enabled {
			return false
		}
	}
	return true
}

// The comparators below exist because gobgpd's ListPeer response is not the
// request we sent it. NewPeerFromConfigStruct builds every sub-message
// unconditionally, so a block the CR omits comes back non-nil and zero-valued;
// several fields are resolved, defaulted or simply never populated. Comparing
// whole messages - with reflect.DeepEqual or proto.Equal, which treat a typed
// nil and an empty message as different - therefore returns false on every
// reconcile and issues an UpdatePeer that changes nothing.
//
// Two rules keep these honest:
//
//  1. A nil sub-message compares equal to an empty one. The CR omitting a block
//     and gobgpd reporting an empty one are the same state.
//  2. Only fields that round-trip are compared. Where gobgpd resolves or derives
//     a value the CR left unset, the CR is not expressing an opinion and we do
//     not compare it. Each exclusion is justified at its site; none of them may
//     be "it was easier".

// timersConfigEqual compares the timer fields gobgpd echoes back.
//
// MinimumAdvertisementInterval is excluded: NewPeerFromConfigStruct never
// populates it, so gobgpd reports 0 no matter what was configured. Comparing it
// would make every reconcile issue an UpdatePeer. The cost is that changing only
// that field is not detected as drift - it still applies on peer creation.
// IdleHoldTimeAfterReset is excluded because gobgpd defaults it (to 30) and the
// CRD has no field for it.
// timersConfigEqual compares each timer only when the CR sets one.
//
// gobgpd defaults every unset timer and echoes the defaulted value, so an
// unconditional comparison is permanently unequal for any timer the CR omits.
// Measured against a live gobgpd v1.3.0 by adding a peer with no timers block
// and reading it back: connect_retry 120, hold_time 90, keepalive_interval 30,
// idle_hold_time_after_reset 30.
//
// connect_retry is the one that bites, because no sample config sets it: a CR
// specifying only holdTime and keepaliveInterval still compared 0 against 120
// and fired UpdatePeer on every reconcile. That was not caught by the
// idempotency tests, whose fake echoed timers as sent rather than as gobgpd
// defaults them - the fake has since been corrected.
//
// The guard is the same one transportConfigEqual uses for LocalAddress and
// routeReflectorEqual for the cluster ID. It carries the same known gap:
// removing a timer from the CR to get the default back is not detected as a
// change. That is rare, and far cheaper than churning every reconcile. It also
// means this does not need to know what the defaults *are*, so it cannot drift
// when the fork changes them.
func timersConfigEqual(desired, current *gobgpapi.Timers) bool {
	d, c := desired.GetConfig(), current.GetConfig()
	if d.GetConnectRetry() != 0 && d.GetConnectRetry() != c.GetConnectRetry() {
		return false
	}
	if d.GetHoldTime() != 0 && d.GetHoldTime() != c.GetHoldTime() {
		return false
	}
	if d.GetKeepaliveInterval() != 0 && d.GetKeepaliveInterval() != c.GetKeepaliveInterval() {
		return false
	}
	return true
}

// transportConfigEqual compares only the controller-managed Transport fields,
// ignoring runtime fields like LocalPort, RemoteAddress, RemotePort, TcpMss.
//
// LocalAddress is compared only when the CR sets one. gobgpd reports
// Transport.State.LocalAddress once a session is established, falling back to
// the configured value, and renders an unset address as the string "invalid IP"
// rather than "". An unconditional comparison is therefore unequal both before
// establishment (""  vs "invalid IP") and after (""  vs the resolved address).
func transportConfigEqual(desired, current *gobgpapi.Transport) bool {
	if desired.GetLocalAddress() != "" && desired.GetLocalAddress() != current.GetLocalAddress() {
		return false
	}
	return desired.GetPassiveMode() == current.GetPassiveMode() &&
		desired.GetBindInterface() == current.GetBindInterface() &&
		desired.GetIpTos() == current.GetIpTos()
}

// applyPolicyEqual compares the policy assignment fields that round-trip.
//
// Name and Direction are both excluded, and they are excluded for opposite
// reasons: crdToAPIPolicyAssignment sets Name and never Direction, while
// gobgpd's newApplyPolicyFromConfigStruct sets Direction and never Name. Neither
// field can ever match, and the direction is already implied by which side of
// the struct the assignment sits on.
func applyPolicyEqual(desired, current *gobgpapi.ApplyPolicy) bool {
	return policyAssignmentEqual(desired.GetImportPolicy(), current.GetImportPolicy()) &&
		policyAssignmentEqual(desired.GetExportPolicy(), current.GetExportPolicy())
}

func policyAssignmentEqual(desired, current *gobgpapi.PolicyAssignment) bool {
	if desired.GetDefaultAction() != current.GetDefaultAction() {
		return false
	}
	dp, cp := desired.GetPolicies(), current.GetPolicies()
	if len(dp) != len(cp) {
		return false
	}
	for i := range dp {
		if dp[i].GetName() != cp[i].GetName() {
			return false
		}
	}
	return true
}

// gracefulRestartEqual compares the three fields the CRD can express.
//
// gobgpd echoes nine, the rest sourced from GracefulRestart.State
// (LocalRestarting, PeerRestartTime, PeerRestarting) or defaulted from its own
// config (DeferralTime, NotificationEnabled, LonglivedEnabled). None of those
// are ours to assert.
func gracefulRestartEqual(desired, current *gobgpapi.GracefulRestart) bool {
	if desired.GetEnabled() != current.GetEnabled() ||
		desired.GetHelperOnly() != current.GetHelperOnly() {
		return false
	}
	// RestartTime is compared only when the CR sets one. gobgpd defaults an unset
	// restart time to HoldTime and echoes the defaulted value - measured against a
	// live gobgp-netlink v1.3.5: sent 0, reported 90 - so comparing it
	// unconditionally is 0 against the hold time for every peer that enables
	// graceful restart without naming a restart time. Same guard, and same known
	// gap, as timersConfigEqual's siblings: clearing restartTime to return to the
	// default is not detected as a change.
	if desired.GetRestartTime() != 0 && desired.GetRestartTime() != current.GetRestartTime() {
		return false
	}
	// staleRoutesTime needs no guard: 0 disables the timer rather than asking for a
	// default, so the daemon echoes it as sent. Measured against v1.3.5, including
	// through peer-group inheritance.
	if desired.GetStaleRoutesTime() != current.GetStaleRoutesTime() {
		return false
	}
	return true
}

// routeReflectorEqual compares the route-reflector settings.
//
// RouteReflectorClusterId is compared only when the CR sets one. gobgpd reports
// RouteReflector.State.RouteReflectorClusterId, which it derives from the router
// ID when a client is configured without an explicit cluster ID, and which
// renders as "invalid IP" when unset.
func routeReflectorEqual(desired, current *gobgpapi.RouteReflector) bool {
	if desired.GetRouteReflectorClusterId() != "" &&
		desired.GetRouteReflectorClusterId() != current.GetRouteReflectorClusterId() {
		return false
	}
	return desired.GetRouteReflectorClient() == current.GetRouteReflectorClient()
}

// ebgpMultihopEqual compares the eBGP multihop settings. Both fields round-trip
// from gobgpd's config unchanged, so this one needs no exclusions - it exists to
// get nil-versus-empty handling via the generated getters.
func ebgpMultihopEqual(desired, current *gobgpapi.EbgpMultihop) bool {
	return desired.GetEnabled() == current.GetEnabled() &&
		desired.GetMultihopTtl() == current.GetMultihopTtl()
}

// definedSetListEqual compares two defined-set member lists exactly.
//
// Verbatim, in configured order, as of gobgp-netlink v1.3.5. The set types keep
// an "originals" slice alongside the compiled regexps - the fork's comment on it
// says "The compiled form is what matches; this is what is reported" - so
// ListDefinedSet hands back precisely the strings the CR supplied.
//
// This used to strip "^" and "$" from both sides, because ParseCommunityRegexp
// canonicalised as it compiled and "65000:100" came back as "^65000:100$".
// Stripping is now wrong in both directions: it is unnecessary, and because
// strings.Trim takes a cutset rather than a prefix and suffix it made "^65000$"
// and "65000" compare equal, so editing a set between those two forms produced
// no RPC at all. The earlier KNOWN GAP about bare integers is gone with it -
// a community written as "4259840" is now reported as "4259840".
func definedSetListEqual(desired, current []string) bool {
	return slices.Equal(desired, current)
}

func definedSetEqual(desired, current *gobgpapi.DefinedSet) bool {
	if desired.GetDefinedType() != current.GetDefinedType() ||
		desired.GetName() != current.GetName() {
		return false
	}
	if !definedSetListEqual(desired.GetList(), current.GetList()) {
		return false
	}
	dp, cp := desired.GetPrefixes(), current.GetPrefixes()
	if len(dp) != len(cp) {
		return false
	}
	for i := range dp {
		if dp[i].GetIpPrefix() != cp[i].GetIpPrefix() ||
			dp[i].GetRtcPrefix() != cp[i].GetRtcPrefix() ||
			dp[i].GetMaskLengthMin() != cp[i].GetMaskLengthMin() ||
			dp[i].GetMaskLengthMax() != cp[i].GetMaskLengthMax() {
			return false
		}
	}
	return true
}

// nodeMatchesSelector checks if this pod's node matches the given label selector.
func (r *BGPConfigurationReconciler) nodeMatchesSelector(ctx context.Context, selector *metav1.LabelSelector, log logr.Logger) (bool, error) {
	node, err := r.getNodeWithRetry(ctx, log)
	if err != nil {
		return false, err
	}
	sel, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return false, err
	}
	return sel.Matches(labels.Set(node.Labels)), nil
}

// hasAnyNodeSelector returns true if any neighbor in the list has a nodeSelector.
func hasAnyNodeSelector(neighbors []bgpv1.Neighbor) bool {
	for i := range neighbors {
		if neighbors[i].NodeSelector != nil {
			return true
		}
	}
	return false
}

// electOwningConfig returns the BGPConfiguration allowed to program gobgpd on
// this node, or nil if there are none.
//
// gobgpd is a singleton per node, but every reconcile function computes its
// desired state from a single CR and then deletes everything gobgpd holds that
// is not in that set. Two BGPConfigurations therefore delete each other's
// defined sets, policies, VRFs, peer groups and neighbors on every pass — and
// a deleted neighbor is a DeletePeer, so gobgpd ends the live session with a
// CEASE / PEER_DECONFIGURED notification. The contract is one BGPConfiguration
// per node; this picks which one it is so the extras can be rejected loudly
// instead of silently fighting.
//
// The oldest config wins, so every node's agent independently reaches the same
// answer with no coordination, and ownership never moves to a config created
// later. Configs being deleted stay eligible: releasing ownership before their
// cleanup has run would let the successor add back the very peers the outgoing
// cleanup is deleting.
func electOwningConfig(configs []bgpv1.BGPConfiguration) *bgpv1.BGPConfiguration {
	var owner *bgpv1.BGPConfiguration
	for i := range configs {
		if owner == nil || configPrecedes(&configs[i], owner) {
			owner = &configs[i]
		}
	}
	return owner
}

// configPrecedes orders BGPConfigurations by creation time, then namespace,
// then name. Creation timestamps have one-second granularity, so the
// namespace/name tie-break is load-bearing rather than defensive: configs
// applied together routinely share a timestamp.
func configPrecedes(a, b *bgpv1.BGPConfiguration) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	if a.Namespace != b.Namespace {
		return a.Namespace < b.Namespace
	}
	return a.Name < b.Name
}

func (r *BGPConfigurationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	startTime := time.Now()
	log := r.Log.WithValues("bgpconfiguration", req.NamespacedName)
	bgpConfig := &bgpv1.BGPConfiguration{}
	if err := r.Get(ctx, req.NamespacedName, bgpConfig); err != nil {
		if apierrors.IsNotFound(err) {
			// The object is gone, so drop its gauges. A config ignored under the
			// one-per-node rule never carries a finalizer, so it is deleted
			// outright and reconcileDelete never runs for it — without this,
			// configuration_ready sits at 0 forever for a config that no longer
			// exists, which reads as "present and not ready". Redundant but
			// harmless for a finalized config, whose cleanup already did this.
			DeleteMetricsForConfig(req.Name, req.Namespace)
			log.Info("BGPConfiguration not found, ignoring")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Decide whether this config owns gobgpd on this node before anything else,
	// including deletion: reconcileDelete removes every object named in the spec
	// unconditionally, so an ignored config must not run it either.
	var configs bgpv1.BGPConfigurationList
	if err := r.List(ctx, &configs); err != nil {
		log.Error(err, "Failed to list BGPConfigurations")
		return ctrl.Result{}, err
	}
	owner := electOwningConfig(configs.Items)
	owned := owner == nil || (owner.Namespace == bgpConfig.Namespace && owner.Name == bgpConfig.Name)

	// Handle deletion with finalizer
	if !bgpConfig.DeletionTimestamp.IsZero() {
		if !owned {
			return r.deleteIgnoredConfig(ctx, bgpConfig, log)
		}
		return r.reconcileDelete(ctx, bgpConfig, log)
	}

	if !owned {
		return r.rejectDuplicateConfig(ctx, bgpConfig, owner, log)
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(bgpConfig, finalizerName) {
		controllerutil.AddFinalizer(bgpConfig, finalizerName)
		if err := r.Update(ctx, bgpConfig); err != nil {
			log.Error(err, "Failed to add finalizer")
			return ctrl.Result{}, err
		}
		// Requeue explicitly: adding a finalizer is a metadata-only change, so
		// generation does not bump and GenerationChangedPredicate filters the
		// resulting Update event. Without this the config is never applied.
		return ctrl.Result{RequeueAfter: time.Second}, nil
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
		if closeErr := conn.Close(); closeErr != nil {
			log.Error(closeErr, "Failed to close gRPC connection")
		}
	}()
	RecordGoBGPConnection(r.getEndpoint(), true)
	apiClient := gobgpapi.NewGoBgpServiceClient(conn)

	// Reconcile all BGP configuration components.
	// desiredPeerGroups and desiredDynNeighbors are declared outside the chain
	// so the post-nodeSelector-filter counts are still in scope for the metric
	// updates below — reporting raw spec lengths there would overstate what is
	// actually configured on this node.
	var (
		reconcileErr        error
		desiredPeerGroups   map[string]*gobgpapi.PeerGroup
		desiredDynNeighbors map[string]*gobgpapi.DynamicNeighbor
	)
	r.reportInertFields(bgpConfig, log)
	if err = r.reconcileGlobal(ctx, apiClient, bgpConfig, log); err != nil {
		reconcileErr = fmt.Errorf("global: %w", err)
	} else if err = r.reconcileDefinedSets(ctx, apiClient, bgpConfig, log); err != nil {
		reconcileErr = fmt.Errorf("defined sets: %w", err)
	} else if err = r.reconcilePolicies(ctx, apiClient, bgpConfig, log); err != nil {
		reconcileErr = fmt.Errorf("policies: %w", err)
	} else if err = r.reconcileVrfs(ctx, apiClient, bgpConfig, log); err != nil {
		reconcileErr = fmt.Errorf("vrfs: %w", err)
	} else if desiredPeerGroups, err = r.reconcilePeerGroups(ctx, apiClient, bgpConfig, log); err != nil {
		reconcileErr = fmt.Errorf("peer groups: %w", err)
	} else if desiredDynNeighbors, err = r.reconcileDynamicNeighbors(ctx, apiClient, bgpConfig, desiredPeerGroups, log); err != nil {
		reconcileErr = fmt.Errorf("dynamic neighbors: %w", err)
	} else if err = r.reconcileNeighbors(ctx, apiClient, bgpConfig, desiredPeerGroups, log); err != nil {
		reconcileErr = fmt.Errorf("neighbors: %w", err)
	} else if err := r.reconcileNetlink(ctx, apiClient, bgpConfig, log); err != nil {
		reconcileErr = fmt.Errorf("netlink: %w", err)
	}

	// Update status
	bgpConfig.Status.ObservedGeneration = bgpConfig.Generation
	now := metav1.Now()
	bgpConfig.Status.LastReconcileTime = &now

	// Update configuration metrics (NeighborCount is set in reconcileNeighbors with post-filter count)
	n, ns := bgpConfig.Name, bgpConfig.Namespace
	UpdateConfiguredObjects(KindNeighbor, n, ns, bgpConfig.Status.NeighborCount)
	UpdateConfiguredObjects(KindPeerGroup, n, ns, len(desiredPeerGroups))
	UpdateConfiguredObjects(KindDynamicNeighbor, n, ns, len(desiredDynNeighbors))
	UpdateConfiguredObjects(KindVrf, n, ns, len(bgpConfig.Spec.Vrfs))
	UpdateConfiguredObjects(KindPolicy, n, ns, len(bgpConfig.Spec.PolicyDefinitions))
	UpdateConfiguredObjects(KindDefinedSet, n, ns, len(bgpConfig.Spec.DefinedSets))

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

	// Pass config to the BGPNodeStatus reporter
	if r.NodeStatusReporter != nil {
		enabled := true
		heartbeat := int32(60)
		if bgpConfig.Spec.Global.NodeStatus != nil {
			if bgpConfig.Spec.Global.NodeStatus.Enabled != nil {
				enabled = *bgpConfig.Spec.Global.NodeStatus.Enabled
			}
			if bgpConfig.Spec.Global.NodeStatus.HeartbeatSeconds != nil {
				heartbeat = *bgpConfig.Spec.Global.NodeStatus.HeartbeatSeconds
			}
		}
		// Get router ID from the cache entry (stored in status during resolveEffectiveRouterID)
		routerID := ""
		routerIDSource := ""
		cacheKey := bgpConfig.Namespace + "/" + bgpConfig.Name
		if entry, ok := r.localRouterIDCache[cacheKey]; ok {
			routerID = entry.routerID
			routerIDSource = entry.source
		}
		r.NodeStatusReporter.UpdateConfig(enabled, heartbeat, routerID, routerIDSource, bgpConfig.Spec.Global.ASN)
	}

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

// rejectDuplicateConfig reports that another BGPConfiguration already owns
// gobgpd on this node and returns without making a single gobgpd call.
//
// No finalizer is added. This config never programmed gobgpd, so there is
// nothing to clean up, and a finalizer would make its deletion run
// reconcileDelete — which deletes unconditionally and would take the owning
// config's neighbors down with it.
func (r *BGPConfigurationReconciler) rejectDuplicateConfig(ctx context.Context, bgpConfig, owner *bgpv1.BGPConfiguration, log logr.Logger) (ctrl.Result, error) {
	msg := fmt.Sprintf("BGPConfiguration %s/%s already manages gobgpd on node %s. Only one BGPConfiguration per node is supported, so this one is ignored and no BGP state is applied from it.",
		owner.Namespace, owner.Name, r.NodeName)
	log.Info("Ignoring BGPConfiguration: another config owns gobgpd on this node",
		"owner", owner.Namespace+"/"+owner.Name)
	if r.Recorder != nil {
		r.Recorder.Event(bgpConfig, corev1.EventTypeWarning, "MultipleConfigurations", msg)
	}

	r.updateStatusCondition(ctx, bgpConfig, "Ready", metav1.ConditionFalse, "MultipleConfigurations", msg)
	bgpConfig.Status.ObservedGeneration = bgpConfig.Generation
	bgpConfig.Status.Message = msg
	now := metav1.Now()
	bgpConfig.Status.LastReconcileTime = &now
	if err := r.Status().Update(ctx, bgpConfig); err != nil {
		log.Error(err, "Failed to update BGPConfiguration status")
	}

	RecordReconcileResult(bgpConfig.Name, bgpConfig.Namespace, "duplicate_ignored")
	UpdateConfigurationReadyStatus(bgpConfig.Name, bgpConfig.Namespace, false)

	// Requeue: nothing enqueues this config when the owning one is deleted, so
	// without a periodic re-check it would stay ignored after inheriting
	// ownership. GenerationChangedPredicate filters the other config's events,
	// and its deletion is not an event on this object at all.
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// deleteIgnoredConfig finishes deletion of a BGPConfiguration that never
// programmed gobgpd, without running the cleanup path.
//
// reconcileDelete deletes every neighbor, peer group, VRF, policy and defined
// set named in the spec unconditionally. Running it for an ignored config would
// delete the owning config's objects out of gobgpd — the same corruption this
// guard exists to prevent, just on the delete path. Reachable for a config that
// still carries a finalizer from before one-config-per-node was enforced.
func (r *BGPConfigurationReconciler) deleteIgnoredConfig(ctx context.Context, bgpConfig *bgpv1.BGPConfiguration, log logr.Logger) (ctrl.Result, error) {
	// Drops the series rejectDuplicateConfig created for this config.
	DeleteMetricsForConfig(bgpConfig.Name, bgpConfig.Namespace)

	if !controllerutil.ContainsFinalizer(bgpConfig, finalizerName) {
		return ctrl.Result{}, nil
	}
	log.Info("Removing finalizer from ignored BGPConfiguration without touching gobgpd")
	controllerutil.RemoveFinalizer(bgpConfig, finalizerName)
	if err := r.Update(ctx, bgpConfig); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
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

		// Delete neighbors first (must be done before peer groups).
		// Delete unconditionally (don't check nodeSelector) using interface or address as appropriate.
		for _, n := range bgpConfig.Spec.Neighbors {
			key := neighborKeyFromCRD(&n.Config)
			if err := deletePeerByKey(ctx, apiClient, &n.Config); err != nil {
				log.Error(err, "Failed to delete neighbor during cleanup", "key", key)
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

		// Disable netlink, if this configuration enabled it.
		//
		// Everything above removes BGP state, which lives and dies with the
		// process. Netlink is the exception: import and export program the
		// *kernel*, and routes this node installed outlive deletion of the CR
		// that asked for them - until the pod happens to restart. Deleting the
		// configuration has to withdraw them, so KeepRoutes is false on both.
		//
		// Failures here are logged but do not join cleanupErrors. A disable of
		// something already disabled is an error from gobgpd, and treating it as
		// a cleanup failure would retry forever and wedge the finalizer,
		// leaving the CR undeletable - a worse outcome than a stale route.
		if bgpConfig.Spec.NetlinkExport != nil && bgpConfig.Spec.NetlinkExport.Enabled {
			if _, err := apiClient.DisableNetlinkExport(ctx, &gobgpapi.DisableNetlinkExportRequest{
				KeepRoutes: false, // flush exported routes from the kernel
			}); err != nil {
				log.V(1).Info("DisableNetlinkExport during cleanup returned error (may not be enabled)", "error", err)
			} else {
				log.Info("Disabled netlink export during cleanup")
			}
		}
		if bgpConfig.Spec.NetlinkImport != nil && bgpConfig.Spec.NetlinkImport.Enabled {
			if _, err := apiClient.DisableNetlinkImport(ctx, &gobgpapi.DisableNetlinkImportRequest{
				KeepRoutes: false, // withdraw imported routes from the RIB
			}); err != nil {
				log.V(1).Info("DisableNetlinkImport during cleanup returned error (may not be enabled)", "error", err)
			} else {
				log.Info("Disabled netlink import during cleanup")
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

		// Drop this CR's router_id_info series. UpdateRouterIDInfo no longer
		// calls Reset(), so nothing else would ever clear it and the series
		// would outlive the CR indefinitely.
		cacheKey := bgpConfig.Namespace + "/" + bgpConfig.Name
		if entry, ok := r.localRouterIDCache[cacheKey]; ok {
			DeleteRouterIDInfo(routerIDInfoLabels(bgpConfig, entry.routerID, entry.source, r.NodeName))
			delete(r.localRouterIDCache, cacheKey)
		}
		// Same reasoning for the global drift series: SetGlobalRestartRequired
		// sets 0 rather than deleting, so a departing CR would leave its series
		// behind at whatever value it last held.
		DeleteGlobalRestartRequired(bgpConfig.Name, bgpConfig.Namespace, globalDriftFields)

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

	// Create desired config with resolved router ID.
	//
	// Everything below the first four fields used to be accepted by the CRD and
	// silently dropped here - a user could set global.gracefulRestart or
	// global.confederation and nothing whatsoever happened.
	families, err := crdToAPIGlobalFamilies(bgpConfig.Spec.Global.Families)
	if err != nil {
		return fmt.Errorf("global.families: %w", err)
	}
	desired := &gobgpapi.Global{
		Asn:                   bgpConfig.Spec.Global.ASN,
		RouterId:              effectiveRouterID,
		ListenPort:            bgpConfig.Spec.Global.ListenPort,
		ListenAddresses:       bgpConfig.Spec.Global.ListenAddresses,
		Families:              families,
		UseMultiplePaths:      bgpConfig.Spec.Global.UseMultiplePaths,
		EbgpMaximumPaths:      bgpConfig.Spec.Global.EbgpMaximumPaths,
		IbgpMaximumPaths:      bgpConfig.Spec.Global.IbgpMaximumPaths,
		RouteSelectionOptions: crdToAPIRouteSelectionOptions(bgpConfig.Spec.Global.RouteSelectionOptions),
		// global.defaultRouteDistance is deliberately not sent: gobgp-netlink
		// v1.3.5 deleted Global.default_route_distance because nothing in the
		// daemon implemented it. The CRD field is retained and inert - see
		// reportInertFields.
		Confederation:   crdToAPIConfederation(bgpConfig.Spec.Global.Confederation),
		GracefulRestart: crdToAPIGracefulRestart(bgpConfig.Spec.Global.GracefulRestart),
		BindToDevice:    bgpConfig.Spec.Global.BindToDevice,
	}

	// Checked before either StartBgp below, not after. gobgpd parses these with
	// netip.ParseAddr and fails the whole StartBgp if one is bad, which surfaces as
	// "BGP server not running" on every reconcile with the real cause buried in the
	// daemon's error. The CRD pattern catches obvious nonsense; this catches the
	// rest, and names the offending value.
	// No "global:" prefix here - Reconcile's chain already wraps this function's
	// error with one, and adding a second produced "global: global: ..." in the
	// logs. Observed on a live cluster, not in a test, because the tests assert on
	// the message rather than reading it.
	if vErr := validateListenAddresses(bgpConfig.Spec.Global.ListenAddresses); vErr != nil {
		return fmt.Errorf("listenAddresses: %w", vErr)
	}
	if vErr := validateMultipath(&bgpConfig.Spec.Global); vErr != nil {
		return vErr
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

	// BGP server already running - check whether it matches the CR.
	//
	// gobgp-netlink v1.3.5 reports every Global field faithfully. Measured by
	// sending a fully-populated Global and reading it back: asn, routerId,
	// listenPort, listenAddresses, families, useMultiplePaths,
	// routeSelectionOptions, confederation, gracefulRestart, bindToDevice,
	// gracefulRestartInheritToNeighbors, ebgpMaximumPaths and ibgpMaximumPaths all
	// come back as sent. An earlier version of this comment claimed only three
	// echoed, which was true of v1.3.0 and is the reason drift went undetected.
	//
	// Three fields are defaulted when not sent, measured on a minimal Global:
	// listenAddresses becomes ["0.0.0.0", "::"], families becomes every supported
	// family, and listenPort becomes 179. Those are guarded on "the CR stated
	// something" for the usual reason - comparing an unset CR value against the
	// daemon's default is permanently unequal. The message-typed fields come back
	// as empty non-nil objects when unset, so the getters compare correctly.
	//
	// ASN and RouterID are the identity of the BGP speaker. gobgpd cannot change
	// them on a running server, and continuing would reconcile neighbors against
	// a speaker that is not the one the CR describes, so this stops.
	if current.GetGlobal().GetAsn() != desired.Asn || current.GetGlobal().GetRouterId() != desired.RouterId {
		log.Info("Global identity changed - pod restart required to apply",
			"currentASN", current.GetGlobal().GetAsn(), "desiredASN", desired.Asn,
			"currentRouterID", current.GetGlobal().GetRouterId(), "desiredRouterID", desired.RouterId)
		return fmt.Errorf("global identity change detected (ASN or RouterID); pod restart required to apply")
	}

	// Everything else in Global also needs a restart to take effect, but it does
	// NOT stop the reconcile. Returning an error here would abort before
	// neighbors, peer groups and policies are reconciled, so one change to a
	// secondary global setting would freeze all other configuration on the node
	// until the pod happened to restart - which is how the first version of this
	// behaved and is strictly worse than applying what can be applied.
	//
	cg := current.GetGlobal()
	drifted := map[string]bool{
		"useMultiplePaths": cg.GetUseMultiplePaths() != desired.UseMultiplePaths,
		"ebgpMaximumPaths": cg.GetEbgpMaximumPaths() != desired.EbgpMaximumPaths,
		"ibgpMaximumPaths": cg.GetIbgpMaximumPaths() != desired.IbgpMaximumPaths,
		"bindToDevice":     cg.GetBindToDevice() != desired.BindToDevice,
		"routeSelectionOptions": !routeSelectionOptionsEqual(
			desired.GetRouteSelectionOptions(), cg.GetRouteSelectionOptions()),
		"confederation":   !confederationEqual(desired.GetConfederation(), cg.GetConfederation()),
		"gracefulRestart": !globalGracefulRestartEqual(desired.GetGracefulRestart(), cg.GetGracefulRestart()),
		// Guarded: the daemon defaults these when the CR states nothing.
		"listenPort":      desired.ListenPort != 0 && cg.GetListenPort() != desired.ListenPort,
		"listenAddresses": len(desired.ListenAddresses) > 0 && !slices.Equal(cg.GetListenAddresses(), desired.ListenAddresses),
		"families":        len(desired.Families) > 0 && !slices.Equal(cg.GetFamilies(), desired.Families),
	}
	UpdateGlobalRestartRequired(bgpConfig, drifted, log, r.Recorder)
	return nil
}

// globalDriftFields is every key UpdateGlobalRestartRequired can report, so the
// series can be cleared on CR deletion without reconstructing the comparison.
var globalDriftFields = []string{
	"useMultiplePaths", "ebgpMaximumPaths", "ibgpMaximumPaths", "bindToDevice",
	"routeSelectionOptions", "confederation", "gracefulRestart", "listenPort",
	"listenAddresses", "families",
}

// UpdateGlobalRestartRequired records secondary global drift and says so once.
//
// Secondary drift does NOT abort the reconcile. Returning an error here would
// stop before neighbors, peer groups and policies are reconciled, so one change
// to a secondary global setting would freeze all other configuration on the node
// until the pod happened to restart - which is how the first version of this
// behaved and is strictly worse than applying what can be applied.
func UpdateGlobalRestartRequired(bgpConfig *bgpv1.BGPConfiguration, drifted map[string]bool, log logr.Logger, rec record.EventRecorder) {
	SetGlobalRestartRequired(bgpConfig.Name, bgpConfig.Namespace, drifted)

	var fields []string
	for _, f := range globalDriftFields {
		if drifted[f] {
			fields = append(fields, f)
		}
	}
	if len(fields) == 0 {
		return
	}
	slices.Sort(fields)
	log.Info("Global settings changed - take effect on next pod restart", "settings", fields)
	if rec == nil {
		return
	}
	rec.Eventf(bgpConfig, corev1.EventTypeWarning, "GlobalRestartRequired",
		"These global settings differ from the running gobgpd and need a pod restart on this node to apply: %s",
		strings.Join(fields, ", "))
}

// validateListenAddresses rejects a listen address gobgpd would reject.
//
// netip.ParseAddr, matching the daemon, rather than net.ParseIP: the two disagree
// on enough inputs that validating with one and running the other is how a value
// passes here and fails at StartBgp. net.ParseIP also accepts a CIDR-looking
// string's address half in some forms, and a listen address is not a prefix.
func validateListenAddresses(addrs []string) error {
	for _, a := range addrs {
		if a == "" {
			return fmt.Errorf("empty address")
		}
		if _, err := netip.ParseAddr(a); err != nil {
			return fmt.Errorf("%q is not a valid IP address: %w", a, err)
		}
	}
	return nil
}

// validateMultipath enforces the pair rule gobgp-netlink v1.3.5 enforces, before
// the daemon does.
//
// The daemon refuses both halves: multipath enabled with no limit selects only the
// single best path, and a limit without multipath is accepted, reported back, and
// does nothing. Its errors name the OpenConfig paths, so this reports the CRD
// fields instead.
//
// This matters more than a nicer message. The failure is at StartBgp, which means
// gobgpd does not start at all - so a CR with useMultiplePaths and no limit takes
// the node's BGP down entirely rather than degrading. Until ebgpMaximumPaths and
// ibgpMaximumPaths existed here there was no way to satisfy the rule, which made
// global.useMultiplePaths unusable on v1.3.5.
func validateMultipath(g *bgpv1.GlobalSpec) error {
	hasLimit := g.EbgpMaximumPaths != 0 || g.IbgpMaximumPaths != 0
	if g.UseMultiplePaths && !hasLimit {
		return fmt.Errorf("useMultiplePaths is enabled but neither ebgpMaximumPaths nor " +
			"ibgpMaximumPaths is set; set at least one, or multipath selects only the single best path")
	}
	if !g.UseMultiplePaths && hasLimit {
		return fmt.Errorf("ebgpMaximumPaths or ibgpMaximumPaths is set but useMultiplePaths is not " +
			"enabled; enable it, or remove the limit")
	}
	return nil
}

// routeSelectionOptionsEqual compares the four fields that survived v1.3.5.
// advertiseInactiveRoutes, enableAigp and ignoreNextHopIgpMetric were deleted
// from the API because nothing implemented them, so there is nothing to compare.
func routeSelectionOptionsEqual(desired, current *gobgpapi.RouteSelectionOptionsConfig) bool {
	return desired.GetAlwaysCompareMed() == current.GetAlwaysCompareMed() &&
		desired.GetIgnoreAsPathLength() == current.GetIgnoreAsPathLength() &&
		desired.GetExternalCompareRouterId() == current.GetExternalCompareRouterId() &&
		desired.GetDisableBestPathSelection() == current.GetDisableBestPathSelection()
}

func confederationEqual(desired, current *gobgpapi.Confederation) bool {
	return desired.GetEnabled() == current.GetEnabled() &&
		desired.GetIdentifier() == current.GetIdentifier() &&
		slices.Equal(desired.GetMemberAsList(), current.GetMemberAsList())
}

// globalGracefulRestartEqual compares the global graceful-restart block.
//
// Named to distinguish it from gracefulRestartEqual, which compares a peer's -
// a different message with different defaulting. This one needs no restartTime
// guard: the daemon echoes the global block as sent rather than defaulting it
// from a hold time, measured against v1.3.5.
func globalGracefulRestartEqual(desired, current *gobgpapi.GracefulRestart) bool {
	return desired.GetEnabled() == current.GetEnabled() &&
		desired.GetRestartTime() == current.GetRestartTime() &&
		desired.GetStaleRoutesTime() == current.GetStaleRoutesTime() &&
		desired.GetDeferralTime() == current.GetDeferralTime()
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
	UpdateRouterIDInfo(nil, routerIDInfoLabels(bgpConfig, resolution.RouterID, resolution.Source, r.NodeName))

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
	// 1. Get current defined sets.
	//
	// ListDefinedSet must be called once per DefinedType. Leaving the field
	// unset means DEFINED_TYPE_UNSPECIFIED, which gobgpd rejects with
	// InvalidArgument — so this listed nothing and logged an error on every
	// reconcile, leaving currentSets empty and every defined set looking new.
	// Only the four types crdToAPIDefinedType can produce are queried.
	currentSets := make(map[string]*gobgpapi.DefinedSet)
	for _, definedType := range []gobgpapi.DefinedType{
		gobgpapi.DefinedType_DEFINED_TYPE_PREFIX,
		gobgpapi.DefinedType_DEFINED_TYPE_NEIGHBOR,
		gobgpapi.DefinedType_DEFINED_TYPE_AS_PATH,
		gobgpapi.DefinedType_DEFINED_TYPE_COMMUNITY,
	} {
		stream, err := apiClient.ListDefinedSet(ctx, &gobgpapi.ListDefinedSetRequest{
			DefinedType: definedType,
		})
		if err != nil {
			return err
		}
		for {
			res, err := stream.Recv()
			if err != nil {
				if err != io.EOF {
					log.Error(err, "error listing defined sets from gobgpd", "definedType", definedType)
				}
				break
			}
			if res.DefinedSet != nil {
				currentSets[res.DefinedSet.Name] = res.DefinedSet
			}
		}
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
			if !definedSetEqual(desired, current) {
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

	// 5. Global policy assignment.
	//
	// global.applyPolicy was accepted by the CRD and never applied. Unlike the
	// other global fields it is not part of the Global message at all - there is
	// no apply_policy on it - so it cannot ride along with StartBgp. Global
	// policy is its own RPC, and nothing in this controller called it.
	//
	// The assignment name is "global", which is what gobgpd keys the
	// global-scope policy off; a neighbor's own applyPolicy still travels on the
	// Peer message and is unaffected by this.
	if err := r.reconcileGlobalPolicyAssignment(ctx, apiClient, bgpConfig, log); err != nil {
		return err
	}
	return nil
}

// reconcileGlobalPolicyAssignment applies spec.global.applyPolicy.
//
// Set unconditionally rather than compared first, matching how policies
// themselves are reconciled just above: SetPolicyAssignment is idempotent, and
// there is no read-back that distinguishes "assigned" from "assigned with the
// same contents" cheaply enough to be worth a comparison here.
func (r *BGPConfigurationReconciler) reconcileGlobalPolicyAssignment(ctx context.Context, apiClient gobgpapi.GoBgpServiceClient, bgpConfig *bgpv1.BGPConfiguration, log logr.Logger) error {
	ap := bgpConfig.Spec.Global.ApplyPolicy
	if ap == nil {
		return nil
	}
	for _, a := range []struct {
		crd       *bgpv1.PolicyAssignment
		direction gobgpapi.PolicyDirection
		label     string
	}{
		{ap.ImportPolicy, gobgpapi.PolicyDirection_POLICY_DIRECTION_IMPORT, "import"},
		{ap.ExportPolicy, gobgpapi.PolicyDirection_POLICY_DIRECTION_EXPORT, "export"},
	} {
		if a.crd == nil {
			continue
		}
		assignment := crdToAPIPolicyAssignment(a.crd)
		assignment.Name = "global"
		assignment.Direction = a.direction
		log.Info("Setting global policy assignment", "direction", a.label, "policies", a.crd.Policies)
		if _, err := apiClient.SetPolicyAssignment(ctx, &gobgpapi.SetPolicyAssignmentRequest{
			Assignment: assignment,
		}); err != nil {
			return fmt.Errorf("set global %s policy assignment: %w", a.label, err)
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

// reconcileDynamicNeighbors returns the filtered set it actually applied, so
// the caller can report a configured-count metric that matches what is on this
// node rather than the raw spec length.
func (r *BGPConfigurationReconciler) reconcileDynamicNeighbors(ctx context.Context, apiClient gobgpapi.GoBgpServiceClient, bgpConfig *bgpv1.BGPConfiguration, desiredPeerGroups map[string]*gobgpapi.PeerGroup, log logr.Logger) (map[string]*gobgpapi.DynamicNeighbor, error) {
	// 1. Get current dynamic neighbors
	currentDynNeighbors := make(map[string]*gobgpapi.DynamicNeighbor)
	stream, err := apiClient.ListDynamicNeighbor(ctx, &gobgpapi.ListDynamicNeighborRequest{})
	if err != nil {
		return nil, err
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

	// 2. Get desired dynamic neighbors (with peer group consistency validation)
	desiredDynNeighbors := make(map[string]*gobgpapi.DynamicNeighbor)
	for _, dn := range bgpConfig.Spec.DynamicNeighbors {
		// Validate peer group reference exists in filtered set
		if _, pgExists := desiredPeerGroups[dn.PeerGroup]; !pgExists {
			log.Info("Skipping dynamic neighbor: referenced peer group not configured on this node",
				"prefix", dn.Prefix, "peerGroup", dn.PeerGroup)
			r.Recorder.Eventf(bgpConfig, corev1.EventTypeWarning, "PeerGroupMissing",
				"Dynamic neighbor %s references peer group %q which is not configured on node %s (filtered by nodeSelector)",
				dn.Prefix, dn.PeerGroup, r.NodeName)
			continue
		}
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
	return desiredDynNeighbors, nil
}

func (r *BGPConfigurationReconciler) reconcilePeerGroups(ctx context.Context, apiClient gobgpapi.GoBgpServiceClient, bgpConfig *bgpv1.BGPConfiguration, log logr.Logger) (map[string]*gobgpapi.PeerGroup, error) {
	// 1. Get current peer groups
	currentPeerGroups := make(map[string]*gobgpapi.PeerGroup)
	stream, err := apiClient.ListPeerGroup(ctx, &gobgpapi.ListPeerGroupRequest{})
	if err != nil {
		return nil, err
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

	// 2. Get desired peer groups (with nodeSelector filtering and password resolution)
	desiredPeerGroups := make(map[string]*gobgpapi.PeerGroup)
	for _, pg := range bgpConfig.Spec.PeerGroups {
		// Filter by nodeSelector
		if pg.NodeSelector != nil {
			matches, err := r.nodeMatchesSelector(ctx, pg.NodeSelector, log)
			if err != nil {
				return nil, fmt.Errorf("peer group %q nodeSelector: %w", pg.Config.PeerGroupName, err)
			}
			if !matches {
				log.V(1).Info("Skipping peer group (nodeSelector doesn't match)", "name", pg.Config.PeerGroupName)
				continue
			}
		}

		// Resolve auth password from Secret or inline value.
		//
		// A peer group is only ever a source of values and never inherits, so
		// api.PeerGroupConf carries no explicit presence and the CRD field stays a
		// plain string. The resolver is shared with the neighbor path, which does
		// need presence, so adapt at the boundary rather than forking it.
		authPassword, err := r.resolveAuthPassword(ctx, bgpConfig.Namespace, &pg.Config.AuthPassword, pg.Config.AuthPasswordSecretRef, log)
		if err != nil {
			return nil, fmt.Errorf("peer group %q: %w", pg.Config.PeerGroupName, err)
		}
		desiredPeerGroups[pg.Config.PeerGroupName] = r.crdToAPIPeerGroupWithPassword(&pg, ptrDeref(authPassword))
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
				RecordPeerApplyError(name, "add_group")
				r.Recorder.Eventf(bgpConfig, corev1.EventTypeWarning, "PeerGroupApplyFailed",
					"Failed to add peer group %s: %v", name, err)
			}
		} else {
			if !peerGroupConfigEqual(desired, current) {
				log.Info("Updating peer group", "name", name)
				if _, err := apiClient.UpdatePeerGroup(ctx, &gobgpapi.UpdatePeerGroupRequest{PeerGroup: desired}); err != nil {
					log.Error(err, "Failed to update peer group", "name", name)
					RecordPeerApplyError(name, "update_group")
					r.Recorder.Eventf(bgpConfig, corev1.EventTypeWarning, "PeerGroupApplyFailed",
						"Failed to update peer group %s: %v", name, err)
				}
			}
		}
	}
	return desiredPeerGroups, nil
}

func (r *BGPConfigurationReconciler) reconcileNeighbors(ctx context.Context, apiClient gobgpapi.GoBgpServiceClient, bgpConfig *bgpv1.BGPConfiguration, desiredPeerGroups map[string]*gobgpapi.PeerGroup, log logr.Logger) error {
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
		currentNeighbors[neighborKey(res.Peer.Conf)] = res.Peer
	}

	// 2. Get desired neighbors (with nodeSelector filtering and password resolution)
	desiredNeighbors := make(map[string]*gobgpapi.Peer)
	inherited := make(map[string][]string)
	for _, n := range bgpConfig.Spec.Neighbors {
		key := neighborKeyFromCRD(&n.Config)

		// Filter by nodeSelector
		if n.NodeSelector != nil {
			matches, err := r.nodeMatchesSelector(ctx, n.NodeSelector, log)
			if err != nil {
				return fmt.Errorf("neighbor %q nodeSelector: %w", key, err)
			}
			if !matches {
				log.V(1).Info("Skipping neighbor (nodeSelector doesn't match)", "key", key)
				continue
			}
		}

		// Validate peer group reference exists in filtered set
		if n.Config.PeerGroup != "" {
			if _, pgExists := desiredPeerGroups[n.Config.PeerGroup]; !pgExists {
				log.Info("Skipping neighbor: referenced peer group not configured on this node",
					"key", key, "peerGroup", n.Config.PeerGroup)
				r.Recorder.Eventf(bgpConfig, corev1.EventTypeWarning, "PeerGroupMissing",
					"Neighbor %s references peer group %q which is not configured on node %s (filtered by nodeSelector)",
					key, n.Config.PeerGroup, r.NodeName)
				continue
			}
		}

		// Resolve auth password from Secret or inline value
		authPassword, err := r.resolveAuthPassword(ctx, bgpConfig.Namespace, n.Config.AuthPassword, n.Config.AuthPasswordSecretRef, log)
		if err != nil {
			return fmt.Errorf("neighbor %q: %w", key, err)
		}
		desired := r.crdToAPINeighborWithPassword(&n, authPassword)
		desiredNeighbors[key] = desired
		if blocks := inheritedBlocks(desired); len(blocks) > 0 {
			inherited[key] = blocks
		}
	}

	// Emit warning if all neighbors were filtered out by nodeSelector
	if len(desiredNeighbors) == 0 && hasAnyNodeSelector(bgpConfig.Spec.Neighbors) {
		r.Recorder.Eventf(bgpConfig, corev1.EventTypeWarning, "NoMatchingNeighbors",
			"No neighbors match node %s labels — check nodeSelector configuration and node labels",
			r.NodeName)
	}

	// Update status with post-filter count
	bgpConfig.Status.NeighborCount = len(desiredNeighbors)

	// 3. Delete unwanted neighbors.
	//
	// Peers accepted from a dynamicNeighbors range are never in desiredNeighbors,
	// which is built solely from spec.neighbors — so they must be excluded here
	// explicitly or every reconcile tears down live dynamic sessions.
	dynPrefixes := dynamicNeighborPrefixes(bgpConfig, log)
	for key, peer := range currentNeighbors {
		if _, ok := desiredNeighbors[key]; ok {
			continue
		}
		if isDynamicallyAcceptedPeer(peer, dynPrefixes) {
			log.V(1).Info("Keeping dynamically-accepted neighbor", "key", key)
			continue
		}
		log.Info("Deleting neighbor", "key", key)
		if peer.Conf.NeighborInterface != "" {
			if _, err := apiClient.DeletePeer(ctx, &gobgpapi.DeletePeerRequest{Interface: peer.Conf.NeighborInterface}); err != nil {
				log.Error(err, "Failed to delete neighbor", "key", key)
			}
		} else {
			if _, err := apiClient.DeletePeer(ctx, &gobgpapi.DeletePeerRequest{Address: peer.Conf.NeighborAddress}); err != nil {
				log.Error(err, "Failed to delete neighbor", "key", key)
			}
		}
	}

	// 4. Add or update neighbors
	for key, desired := range desiredNeighbors {
		if current, ok := currentNeighbors[key]; !ok {
			log.Info("Adding neighbor", "key", key)
			if _, err := apiClient.AddPeer(ctx, &gobgpapi.AddPeerRequest{Peer: desired}); err != nil {
				// Counted, not just logged: gobgpd refusing a peer leaves no
				// trace in its own metrics - the neighbor is simply absent,
				// which looks the same as never having been configured.
				log.Error(err, "Failed to add neighbor", "key", key)
				RecordPeerApplyError(key, "add")
				r.Recorder.Eventf(bgpConfig, corev1.EventTypeWarning, "PeerApplyFailed",
					"Failed to add neighbor %s: %v", key, err)
			}
		} else {
			r.reportPeerAsnOverridden(bgpConfig, key, desired, current, log)
			if !peerConfigEqual(desired, current) || r.authPasswordDrifted(bgpConfig, key, desired, current, log) {
				log.Info("Updating neighbor", "key", key)
				if _, err := apiClient.UpdatePeer(ctx, &gobgpapi.UpdatePeerRequest{Peer: desired}); err != nil {
					log.Error(err, "Failed to update neighbor", "key", key)
					RecordPeerApplyError(key, "update")
					r.Recorder.Eventf(bgpConfig, corev1.EventTypeWarning, "PeerApplyFailed",
						"Failed to update neighbor %s: %v", key, err)
				}
			}
		}
	}

	// Hand the reporter what only this function knows. Set unconditionally, so a
	// neighbor that stops inheriting - or leaves its group - loses its entry rather
	// than keeping a stale one.
	if r.NodeStatusReporter != nil {
		r.NodeStatusReporter.UpdateInheritedBlocks(inherited)
	}
	return nil
}

// reportInertFields warns once per reconcile for each inert field the CR sets.
//
// The six fields it looks for are the ones gobgp-netlink v1.3.5 removed from its
// API. All were accepted by the daemon and implemented by nothing, which is why
// the fork deleted them; setting one has never had an effect. They stay in the
// CRD because removing a field from a structural schema makes the apiserver
// prune it silently - kubectl apply succeeds, no error, the value vanishes - and
// a loud warning is more use than a quiet deletion.
//
// This is a secondary signal, not the primary one. Reconcile is driven by spec
// generation changes, so a CR that set one of these fields long ago and is never
// edited produces no event until the agent restarts, and Events age out. The
// durable check is the pre-flight query in the upgrade notes.
func (r *BGPConfigurationReconciler) reportInertFields(bgpConfig *bgpv1.BGPConfiguration, log logr.Logger) {
	var set []string
	g := bgpConfig.Spec.Global
	if g.DefaultRouteDistance != nil {
		set = append(set, "global.defaultRouteDistance")
	}
	if rso := g.RouteSelectionOptions; rso != nil {
		if rso.AdvertiseInactiveRoutes {
			set = append(set, "global.routeSelectionOptions.advertiseInactiveRoutes")
		}
		if rso.EnableAigp {
			set = append(set, "global.routeSelectionOptions.enableAigp")
		}
		if rso.IgnoreNextHopIgpMetric {
			set = append(set, "global.routeSelectionOptions.ignoreNextHopIgpMetric")
		}
	}
	for _, n := range bgpConfig.Spec.Neighbors {
		if n.Config.RouteFlapDamping {
			set = append(set, "neighbors[].config.routeFlapDamping")
			break
		}
	}
	for _, pg := range bgpConfig.Spec.PeerGroups {
		if pg.Config.RouteFlapDamping {
			set = append(set, "peerGroups[].config.routeFlapDamping")
			break
		}
	}
	for _, n := range bgpConfig.Spec.Neighbors {
		if n.Timers != nil && n.Timers.Config.MinimumAdvertisementInterval != 0 {
			set = append(set, "neighbors[].timers.config.minimumAdvertisementInterval")
			break
		}
	}
	for _, pg := range bgpConfig.Spec.PeerGroups {
		if pg.Timers != nil && pg.Timers.Config.MinimumAdvertisementInterval != 0 {
			set = append(set, "peerGroups[].timers.config.minimumAdvertisementInterval")
			break
		}
	}
	if len(set) == 0 {
		return
	}
	log.Info("Ignoring settings that gobgp-netlink no longer implements", "fields", set)
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(bgpConfig, corev1.EventTypeWarning, "InertSettings",
		"These settings are accepted by the CRD but not implemented by gobgpd and have no effect: %s",
		strings.Join(set, ", "))
}

// authPasswordDrifted reports whether gobgpd is using a TCP-MD5 password when the
// CR says it should not, or vice versa.
//
// peerConfigEqual cannot compare the password itself: ListPeer redacts it to a
// non-nil empty string, so the desired value never matches and comparing it would
// mean an UpdatePeer every reconcile for every authenticated peer. Presence is
// comparable though - PeerState.AuthPasswordSet is a plain bool the daemon
// computes from the resolved config, so it round-trips and cannot churn.
//
// Returning true forces the update, which re-sends the password and repairs the
// session. That is worth doing even though the event is the more useful signal:
// a peer silently running unauthenticated is the failure mode here, and for a
// grouped peer whose only owned field is its password nothing else would ever
// trigger a correction.
func (r *BGPConfigurationReconciler) authPasswordDrifted(bgpConfig *bgpv1.BGPConfiguration, key string, desired, current *gobgpapi.Peer, log logr.Logger) bool {
	// Nil means the CR stated nothing, so the peer group's password - if any -
	// applies and there is nothing here to compare against. A non-nil empty string
	// is different: it is a deliberate opt-out, and the daemon should report no
	// password set.
	if desired.GetConf().AuthPassword == nil {
		return false
	}
	want := desired.GetConf().GetAuthPassword() != ""
	have := current.GetState().GetAuthPasswordSet()
	if want == have {
		return false
	}
	log.Info("TCP-MD5 authentication state differs from the CR",
		"key", key, "configured", want, "inEffect", have)
	if r.Recorder == nil {
		return true
	}
	r.Recorder.Eventf(bgpConfig, corev1.EventTypeWarning, "AuthPasswordDrift",
		"Neighbor %s: CR specifies TCP-MD5 authentication=%t but gobgpd reports %t; re-sending the configuration.",
		key, want, have)
	return true
}

// inheritedBlocks names the blocks this neighbor will take from its peer group.
//
// Derived from what the CR stated, which is the only place that knows: ListPeer
// reports the resolved configuration, so a value the neighbor set and one it
// inherited are indistinguishable there. Exactly the predicate peerConfigEqual uses
// to decide what not to compare, so the two cannot disagree about which blocks a
// group owns.
//
// Kept in sync with the skip list in peerConfigEqual. Re-derive both from the
// PeerGroup type in api/v1/types.go rather than from either comment.
func inheritedBlocks(desired *gobgpapi.Peer) []string {
	if desired.GetConf().GetPeerGroup() == "" {
		return nil
	}
	var blocks []string
	if len(desired.GetAfiSafis()) == 0 {
		blocks = append(blocks, "afiSafis")
	}
	if desired.ApplyPolicy == nil {
		blocks = append(blocks, "applyPolicy")
	}
	if desired.Timers == nil {
		blocks = append(blocks, "timers")
	}
	if desired.Transport == nil {
		blocks = append(blocks, "transport")
	}
	if desired.GracefulRestart == nil {
		blocks = append(blocks, "gracefulRestart")
	}
	if desired.Bfd == nil {
		blocks = append(blocks, "bfd")
	}
	slices.Sort(blocks)
	return blocks
}

// reportPeerAsnOverridden says so when a peer group has discarded a grouped
// neighbor's stated peerAsn.
//
// peer-as is the one field left in oc's forcedOverwrittenConfig, so where a peer
// group states one the group wins and UpdatePeer cannot change it.
// peerConfigEqual therefore skips the comparison for grouped peers - otherwise
// every reconcile would issue an update that cannot take effect. That skip
// removes the only symptom an operator had, so replace it with a deliberate
// signal: the CR names an ASN, the daemon reports a different one, and the
// difference is fixable by editing the CR or the group.
//
// gobgp-netlink v1.3.5 no longer zeroes a member's peerAsn when the group states
// none, so this fires only on a genuine group-versus-member disagreement.
func (r *BGPConfigurationReconciler) reportPeerAsnOverridden(bgpConfig *bgpv1.BGPConfiguration, key string, desired, current *gobgpapi.Peer, log logr.Logger) {
	dc, cc := desired.GetConf(), current.GetConf()
	if dc.GetPeerGroup() == "" || dc.GetPeerAsn() == cc.GetPeerAsn() {
		return
	}
	log.Info("Peer group overrode neighbor peerAsn",
		"key", key, "peerGroup", dc.GetPeerGroup(),
		"configured", dc.GetPeerAsn(), "inEffect", cc.GetPeerAsn())
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(bgpConfig, corev1.EventTypeWarning, "PeerAsnOverridden",
		"Neighbor %s specifies peerAsn %d but peer group %q supplies %d; the group's value is used. Set peerAsn on the group, or remove the neighbor from it.",
		key, dc.GetPeerAsn(), dc.GetPeerGroup(), cc.GetPeerAsn())
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
	// slices.Equal, not reflect.DeepEqual: an interfaceList of [] in the CR is a
	// non-nil empty slice while gobgpd reports nil, and DeepEqual calls those
	// different - which disables and re-enables the importer on every reconcile.
	if desiredImportEnabled && !slices.Equal(desiredInterfaces, currentNetlink.Interfaces) {
		needsImportUpdate = true
	}
	// The VRF is part of the import configuration, and GetNetlink reports it, but
	// it was fetched and discarded - so changing only netlinkImport.vrf in the CR
	// was silently ignored. This is the inverse of the churn above: not an update
	// that does nothing, but a change that never happens.
	if desiredImportEnabled && desiredVrf != currentNetlink.Vrf {
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
	//
	// dampeningInterval and routeProtocol are compared as of gobgp-netlink
	// v1.3.5, which added both to GetNetlinkResponse. Measured against a live
	// daemon: sent 250/186, reported 250/186. Before that there was nothing to
	// compare a changed CR value against and editing either alone was silently
	// ignored until something else forced an update.
	//
	// Guarded on non-zero because the daemon defaults an unset dampeningInterval
	// to 100 and an unset routeProtocol to 186 (RTPROT_BGP) and reports the
	// defaulted value, so comparing a CR that omits them would be 0 against the
	// default - a permanent update loop, which is the failure the old note warned
	// about. Clearing either back to the default is therefore not detected, the
	// same known gap as the other defaulted fields.
	needsExportUpdate := false
	if desiredExportEnabled != currentNetlink.ExportEnabled {
		needsExportUpdate = true
	}
	if desiredExportEnabled && bgpConfig.Spec.NetlinkExport != nil {
		e := bgpConfig.Spec.NetlinkExport
		if e.DampeningInterval != 0 && e.DampeningInterval != currentNetlink.GetDampeningInterval() {
			needsExportUpdate = true
		}
		if e.RouteProtocol != 0 && e.RouteProtocol != currentNetlink.GetRouteProtocol() {
			needsExportUpdate = true
		}
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

	// Per-VRF export state, keyed by GoBGP VRF name. Without this the export
	// branch below has nothing to compare against and re-issues
	// EnableVrfNetlinkExport on every reconcile - an Info log and an RPC per VRF
	// per cycle, forever.
	currentVrfExports := make(map[string]*gobgpapi.ListNetlinkExportRulesResponse_VrfExportRule)
	if rules, err := apiClient.ListNetlinkExportRules(ctx, &gobgpapi.ListNetlinkExportRulesRequest{}); err != nil {
		// Not fatal: fall back to the previous unconditional behavior rather
		// than failing the whole reconcile over a read-back.
		log.V(1).Info("Failed to list netlink export rules; VRF export will be re-applied unconditionally", "error", err)
	} else {
		for _, vr := range rules.GetVrfRules() {
			currentVrfExports[vr.GetGobgpVrf()] = vr
		}
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
			currentImportEnabled := current.GetNetlink().GetImportEnabled()

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
				// Check if interfaces changed. slices.Equal, not DeepEqual: see
				// the note in reconcileNetlink - nil and empty are the same state
				// here, and DeepEqual churns the importer if they differ.
				if !slices.Equal(vrf.NetlinkImport.InterfaceList, current.GetNetlink().GetImportInterfaces()) {
					// Re-enable with new interfaces (disable first, then enable)
					log.Info("Updating VRF netlink import interfaces",
						"vrf", vrf.Name,
						"currentInterfaces", current.GetNetlink().GetImportInterfaces(),
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

		// Handle VRF netlink export, against the read-back gathered above.
		currentExport, exportPresent := currentVrfExports[vrf.Name]
		if vrf.NetlinkExport != nil && vrf.NetlinkExport.Enabled {
			if exportPresent && vrfNetlinkExportEqual(vrf.NetlinkExport, currentExport) {
				continue
			}
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
		} else if vrf.NetlinkExport != nil && !vrf.NetlinkExport.Enabled && exportPresent {
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

// vrfNetlinkExportEqual reports whether a VRF's desired export config already
// matches what gobgpd has.
//
// Note the polarity flip: the request carries SkipNexthopValidation while the
// read-back carries ValidateNexthop, and the CRD's ValidateNexthop defaults to
// true when unset - so all three have to be normalized to the same sense before
// they can be compared.
//
// LinuxVrf and LinuxTableId are compared only when the CR sets them: gobgpd
// defaults LinuxVrf to the GoBGP VRF name and looks LinuxTableId up from the
// Linux VRF, so an unset CR field is not an assertion that they are empty.
func vrfNetlinkExportEqual(desired *bgpv1.VrfNetlinkExport, current *gobgpapi.ListNetlinkExportRulesResponse_VrfExportRule) bool {
	if desired.LinuxVrf != "" && desired.LinuxVrf != current.GetLinuxVrf() {
		return false
	}
	if desired.LinuxTableId != 0 && desired.LinuxTableId != current.GetLinuxTableId() {
		return false
	}
	if desired.Metric != current.GetMetric() {
		return false
	}
	validateNexthop := true // CRD default when unset
	if desired.ValidateNexthop != nil {
		validateNexthop = *desired.ValidateNexthop
	}
	if validateNexthop != current.GetValidateNexthop() {
		return false
	}
	return slices.Equal(desired.CommunityList, current.GetCommunityList()) &&
		slices.Equal(desired.LargeCommunityList, current.GetLargeCommunityList())
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
		apiRules = append(apiRules, &gobgpapi.NetlinkExportRuleConfig{
			Name:               rule.Name,
			CommunityList:      rule.CommunityList,
			LargeCommunityList: rule.LargeCommunityList,
			Vrf:                rule.Vrf,
			TableId:            rule.TableId,
			Metric:             rule.Metric,
			// Passed through: gobgp-netlink defaults an absent
			// validate_nexthop to true, which is what the local default below
			// used to do by hand. Measured against v1.3.5: nil -> validate,
			// true -> validate, false -> skip.
			ValidateNexthop: rule.ValidateNexthop,
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

		// Compare community lists. slices.Equal, not DeepEqual: a rule with
		// communityList: [] in the CR is non-nil empty where gobgpd reports nil,
		// and DeepEqual would re-issue EnableNetlinkExport on every reconcile.
		if !slices.Equal(currentRule.CommunityList, desiredRule.CommunityList) {
			return false
		}
		if !slices.Equal(currentRule.LargeCommunityList, desiredRule.LargeCommunityList) {
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
			RtcPrefix:     p.RtcPrefix,
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
	out := &gobgpapi.Conditions{
		PrefixSet:         crdToAPIMatchSet(crd.PrefixSet),
		NeighborSet:       crdToAPIMatchSet(crd.NeighborSet),
		AsPathSet:         crdToAPIMatchSet(crd.AsPathSet),
		CommunitySet:      crdToAPIMatchSet(crd.CommunitySet),
		ExtCommunitySet:   crdToAPIMatchSet(crd.ExtCommunitySet),
		LargeCommunitySet: crdToAPIMatchSet(crd.LargeCommunitySet),
		RpkiResult:        crdToAPIRpkiValidationResult(crd.RpkiResult),
		AsPathLength:      crdToAPIAsPathLength(crd.AsPathLength),
		CommunityCount:    crdToAPICommunityCount(crd.CommunityCount),
		RouteType:         crdToAPIRouteType(crd.RouteType),
		Origin:            crdToAPIOriginType(crd.Origin),
		NextHopInList:     crd.NextHopInList,
		AfiSafiIn:         crdToAPIFamilies(crd.AfiSafiIn),
	}
	// Pointers in the CRD because 0 is a legitimate value for both, and the
	// proto wraps each in a message whose presence is the "match on this at
	// all" signal.
	if crd.LocalPrefEq != nil {
		out.LocalPrefEq = &gobgpapi.LocalPrefEq{Value: *crd.LocalPrefEq}
	}
	if crd.MedEq != nil {
		out.MedEq = &gobgpapi.MedEq{Value: *crd.MedEq}
	}
	return out
}

func crdToAPIActions(crd *bgpv1.Actions) *gobgpapi.Actions {
	out := &gobgpapi.Actions{
		RouteAction:    crdToAPIRouteAction(crd.RouteAction),
		Community:      crdToAPICommunityAction(crd.Community),
		ExtCommunity:   crdToAPICommunityAction(crd.ExtCommunity),
		LargeCommunity: crdToAPICommunityAction(crd.LargeCommunity),
		Med:            crdToAPIMedAction(crd.Med),
		AsPrepend:      crdToAPIAsPrependAction(crd.AsPrepend),
		Nexthop:        crdToAPINexthopAction(crd.Nexthop),
		LocalPref:      &gobgpapi.LocalPrefAction{Value: crd.LocalPref},
	}
	if crd.Origin != "" {
		out.OriginAction = &gobgpapi.OriginAction{Origin: crdToAPIOriginType(crd.Origin)}
	}
	return out
}

// crdToAPIPeerGroupWithPassword converts a CRD PeerGroup to API PeerGroup with resolved password
func (r *BGPConfigurationReconciler) crdToAPIPeerGroupWithPassword(crd *bgpv1.PeerGroup, authPassword string) *gobgpapi.PeerGroup {
	return &gobgpapi.PeerGroup{
		Conf: &gobgpapi.PeerGroupConf{
			PeerGroupName:        crd.Config.PeerGroupName,
			PeerAsn:              crd.Config.PeerAsn,
			LocalAsn:             crd.Config.LocalAsn,
			Description:          crd.Config.Description,
			AuthPassword:         authPassword,
			AllowOwnAsn:          crd.Config.AllowOwnAsn,
			ReplacePeerAsn:       crd.Config.ReplacePeerAsn,
			AllowAspathLoopLocal: crd.Config.AllowAspathLoopLocal,
			RemovePrivate:        crdToAPIRemovePrivate(crd.Config.RemovePrivate),
			SendSoftwareVersion:  crd.Config.SendSoftwareVersion,
			SendCommunity:        crdToAPISendCommunity(crd.Config.SendCommunity),
		},
		AfiSafis:        crdToAPIAfiSafis(crd.AfiSafis),
		ApplyPolicy:     crdToAPIApplyPolicy(crd.ApplyPolicy),
		Timers:          crdToAPITimers(crd.Timers),
		Transport:       crdToAPITransport(crd.Transport),
		GracefulRestart: crdToAPIGracefulRestart(crd.GracefulRestart),
		Bfd:             crdToAPIBfd(crd.BFD),
	}
}

// crdToAPINeighborWithPassword converts a CRD Neighbor to API Peer with resolved password
func (r *BGPConfigurationReconciler) crdToAPINeighborWithPassword(crd *bgpv1.Neighbor, authPassword *string) *gobgpapi.Peer {
	return &gobgpapi.Peer{
		Conf: &gobgpapi.PeerConf{
			NeighborAddress:   crd.Config.NeighborAddress,
			PeerAsn:           crd.Config.PeerAsn,
			PeerGroup:         crd.Config.PeerGroup,
			AdminDown:         crd.Config.AdminDown,
			NeighborInterface: crd.Config.NeighborInterface,
			Vrf:               crd.Config.Vrf,
			// Explicit presence as of gobgp-netlink v1.3.5: nil means "inherit
			// from my peer group", a pointer means "I own this". See ptrIfSet.
			//
			// AllowOwnAsn, ReplacePeerAsn and AllowAspathLoopLocal share one
			// block-level presence guard on the daemon side, so stating any one
			// of them claims all three and the other two stop inheriting.
			// Passed straight through: these CRD fields are pointers, so the CR
			// itself carries the presence the daemon reads. Omitted means nil
			// means inherit; stated - including stated as the zero value - claims
			// the field. localAsn stays a zero test because 0 already means "use
			// the global ASN", so unset and explicit zero are the same request.
			LocalAsn:             ptrIfSet(crd.Config.LocalAsn),
			Description:          crd.Config.Description,
			AuthPassword:         authPassword,
			AllowOwnAsn:          crd.Config.AllowOwnAsn,
			ReplacePeerAsn:       crd.Config.ReplacePeerAsn,
			AllowAspathLoopLocal: crd.Config.AllowAspathLoopLocal,
			RemovePrivate:        crdToAPIRemovePrivatePtr(crd.Config.RemovePrivate),
			SendSoftwareVersion:  crd.Config.SendSoftwareVersion,
			SendCommunity:        crdToAPISendCommunity(crd.Config.SendCommunity),
		},
		AfiSafis:        crdToAPIAfiSafis(crd.AfiSafis),
		ApplyPolicy:     crdToAPIApplyPolicy(crd.ApplyPolicy),
		Timers:          crdToAPITimers(crd.Timers),
		Transport:       crdToAPITransport(crd.Transport),
		GracefulRestart: crdToAPIGracefulRestart(crd.GracefulRestart),
		RouteReflector:  crdToAPIRouteReflector(crd.RouteReflector),
		EbgpMultihop:    crdToAPIEbgpMultihop(crd.EbgpMultihop),
		Bfd:             crdToAPIBfd(crd.BFD),
	}
}

// crdToAPIBfd converts a CRD BFD block. A nil block stays nil, which is what
// tells gobgpd to inherit the peer group's - inheritance is whole-block, keyed
// on the block being entirely zero.
func crdToAPIBfd(crd *bgpv1.BFD) *gobgpapi.BfdPeerConfig {
	if crd == nil {
		return nil
	}
	enabled := false
	if crd.Enabled != nil {
		enabled = *crd.Enabled
	}
	return &gobgpapi.BfdPeerConfig{
		Enabled:                  enabled,
		Port:                     crd.Port,
		DesiredMinimumTxInterval: crd.DesiredMinimumTxInterval,
		RequiredMinimumReceive:   crd.RequiredMinimumReceive,
		DetectionMultiplier:      crd.DetectionMultiplier,
	}
}

// gobgpd's BFD defaults, applied by SetDefaultNeighborConfigValues to every
// neighbor regardless of whether BFD is enabled. They must match the CRD's
// kubebuilder defaults exactly, or a peer that sets a bfd block compares
// unequal against what ListPeer echoes back and churns forever.
const (
	bfdDefaultPort                = 3784
	bfdDefaultInterval            = 1000000 // microseconds
	bfdDefaultDetectionMultiplier = 3
)

// bfdWithDefaults returns cfg with gobgpd's defaults filled in.
//
// Applied unconditionally, including to a nil or all-zero block: gobgpd defaults
// every neighbor's BFD config whether or not BFD is enabled, and
// NewPeerFromConfigStruct then emits it for every peer. So the case that churns
// is not a partially-specified block - it is a peer with no BFD at all, where
// the CR sends nothing and ListPeer returns a fully-populated one.
func bfdWithDefaults(cfg *gobgpapi.BfdPeerConfig) *gobgpapi.BfdPeerConfig {
	out := &gobgpapi.BfdPeerConfig{
		Enabled:                  cfg.GetEnabled(),
		Port:                     cfg.GetPort(),
		DesiredMinimumTxInterval: cfg.GetDesiredMinimumTxInterval(),
		RequiredMinimumReceive:   cfg.GetRequiredMinimumReceive(),
		DetectionMultiplier:      cfg.GetDetectionMultiplier(),
	}
	if out.Port == 0 {
		out.Port = bfdDefaultPort
	}
	if out.DetectionMultiplier == 0 {
		out.DetectionMultiplier = bfdDefaultDetectionMultiplier
	}
	if out.DesiredMinimumTxInterval == 0 {
		out.DesiredMinimumTxInterval = bfdDefaultInterval
	}
	if out.RequiredMinimumReceive == 0 {
		out.RequiredMinimumReceive = bfdDefaultInterval
	}
	return out
}

// bfdEqual compares two BFD configs as gobgpd would report them.
//
// defaulted fills gobgpd's defaults on *both* sides before comparing, and it is
// required for peers: addNeighbor runs SetDefaultNeighborConfigValues, so
// ListPeer echoes 3784/3/1e6/1e6 against the nil block the CR sent, and without
// it every neighbor without BFD churns.
//
// For peer groups it is false because addPeerGroup runs no defaulting and
// ListPeerGroup echoes the block as sent. Measured honestly: passing true here
// would *also* compare equal today, since both sides receive the same treatment
// and the CRD's own defaults mean a partial block never reaches this code. It is
// false because that is what the daemon actually does - so if the fork starts
// defaulting peer groups, the peer path is already correct and this one fails
// loudly rather than silently drifting.
func bfdEqual(desired, current *gobgpapi.BfdPeerConfig, defaulted bool) bool {
	d, c := desired, current
	if defaulted {
		d, c = bfdWithDefaults(desired), bfdWithDefaults(current)
	}
	return d.GetEnabled() == c.GetEnabled() &&
		d.GetPort() == c.GetPort() &&
		d.GetDesiredMinimumTxInterval() == c.GetDesiredMinimumTxInterval() &&
		d.GetRequiredMinimumReceive() == c.GetRequiredMinimumReceive() &&
		d.GetDetectionMultiplier() == c.GetDetectionMultiplier()
}

// --- Secret Resolution Helper ---

// resolveAuthPassword resolves the authentication password from either inline value or Secret reference.
// Returns an error if a SecretRef is specified but the Secret or key cannot be found.
// resolveAuthPassword returns the password to send, or nil when the CR states
// none.
//
// The nil is load-bearing: api.PeerConf.auth_password carries explicit presence,
// so nil lets a peer group supply the password while a non-nil empty string is an
// explicit opt-out from it. A plain "" could not express that difference, which is
// why authPassword became a *string in the CRD.
func (r *BGPConfigurationReconciler) resolveAuthPassword(ctx context.Context, namespace string, authPassword *string, secretRef *corev1.SecretKeySelector, log logr.Logger) (*string, error) {
	// If SecretRef is specified, prefer it over inline password
	if secretRef != nil && secretRef.Name != "" {
		secret := &corev1.Secret{}
		secretName := types.NamespacedName{
			Namespace: namespace,
			Name:      secretRef.Name,
		}
		if err := r.Get(ctx, secretName, secret); err != nil {
			return nil, fmt.Errorf("failed to get Secret %q for auth password: %w", secretRef.Name, err)
		}
		if password, ok := secret.Data[secretRef.Key]; ok {
			p := string(password)
			return &p, nil
		}
		return nil, fmt.Errorf("key %q not found in Secret %q", secretRef.Key, secretRef.Name)
	}
	// Fall back to inline password (deprecated). Passed through as-is, nil
	// included, so "stated nothing" stays distinguishable from "stated empty".
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
			// PrefixLimit carries its own Family, and gobgpd matches the limit
			// to the afi-safi by it, so it has to be the same family as the
			// entry it sits on rather than left nil.
			PrefixLimits: crdToAPIPrefixLimit(crd.PrefixLimit, crd.Family),
			AddPaths:     crdToAPIAddPaths(crd.AddPaths),
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
			ConnectRetry:      crd.Config.ConnectRetry,
			HoldTime:          crd.Config.HoldTime,
			KeepaliveInterval: crd.Config.KeepaliveInterval,
			// timers.minimumAdvertisementInterval is deliberately not sent:
			// gobgp-netlink v1.3.5 deleted TimersConfig.
			// minimum_advertisement_interval because nothing read it. The CRD
			// field is retained and inert - see reportInertFields.
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
		IpTos:         crd.IpTos,
	}
}

// bgpFamily is one address family as this CRD names it, with both encodings
// gobgpd needs for it.
//
// One table, because there are two completely different encodings and three
// call sites, and keeping them in separate lists is how they drift:
//
//   - afiSafi carries the Afi/Safi pair used by Peer.AfiSafis, PrefixLimit and
//     policy conditions.
//   - globalOrdinal is the bare index Global.Families expects. It is NOT
//     afi<<16|safi: sending 65537 for ipv4-unicast does not error, it enables
//     no families at all and the node carries no routes. Measured against a
//     live gobgpd v1.3.0 by starting one daemon per candidate value and asking
//     GetTable which families existed. TestGlobalFamilyOrdinals pins it.
//
// hasGlobalOrdinal is false for families that Global.Families cannot express,
// so they are rejected there rather than silently mapped to the wrong index.
type bgpFamily struct {
	afi              gobgpapi.Family_Afi
	safi             gobgpapi.Family_Safi
	globalOrdinal    uint32
	hasGlobalOrdinal bool
}

// bgpFamilies is the canonical set. The CRD enums on global.families,
// afiSafis[].family and conditions.afiSafiIn are generated from these names and
// must stay in step with them.
var bgpFamilies = map[string]bgpFamily{
	"ipv4-unicast": {gobgpapi.Family_AFI_IP, gobgpapi.Family_SAFI_UNICAST, 0, true},
	"ipv6-unicast": {gobgpapi.Family_AFI_IP6, gobgpapi.Family_SAFI_UNICAST, 1, true},
	"ipv4-labeled": {gobgpapi.Family_AFI_IP, gobgpapi.Family_SAFI_MPLS_LABEL, 2, true},
	"ipv6-labeled": {gobgpapi.Family_AFI_IP6, gobgpapi.Family_SAFI_MPLS_LABEL, 3, true},
	"ipv4-vpn":     {gobgpapi.Family_AFI_IP, gobgpapi.Family_SAFI_MPLS_VPN, 4, true},
	"ipv6-vpn":     {gobgpapi.Family_AFI_IP6, gobgpapi.Family_SAFI_MPLS_VPN, 5, true},
	"l2vpn-vpls":   {gobgpapi.Family_AFI_L2VPN, gobgpapi.Family_SAFI_VPLS, 8, true},
	"l2vpn-evpn":   {gobgpapi.Family_AFI_L2VPN, gobgpapi.Family_SAFI_EVPN, 9, true},
	// Expressible per-neighbor but not in Global.Families.
	"ipv4-flowspec": {gobgpapi.Family_AFI_IP, gobgpapi.Family_SAFI_FLOW_SPEC_UNICAST, 0, false},
	"ipv6-flowspec": {gobgpapi.Family_AFI_IP6, gobgpapi.Family_SAFI_FLOW_SPEC_UNICAST, 0, false},
	"rtc":           {gobgpapi.Family_AFI_IP, gobgpapi.Family_SAFI_ROUTE_TARGET_CONSTRAINTS, 0, false},
}

// crdToAPIGlobalFamilies converts CRD family names to Global.Families.
//
// An empty list returns nil, which leaves gobgpd's default of ipv4-unicast and
// ipv6-unicast. An unknown or non-global family is an error rather than a
// skipped entry: the zero ordinal is ipv4-unicast, so silently dropping or
// defaulting one would hand back a family the operator did not ask for.
func crdToAPIGlobalFamilies(names []string) ([]uint32, error) {
	if len(names) == 0 {
		return nil, nil
	}
	out := make([]uint32, 0, len(names))
	for _, n := range names {
		f, ok := bgpFamilies[strings.ToLower(n)]
		if !ok {
			return nil, fmt.Errorf("unknown address family %q", n)
		}
		if !f.hasGlobalOrdinal {
			return nil, fmt.Errorf("address family %q cannot be set in global.families; enable it per-neighbor with afiSafis", n)
		}
		out = append(out, f.globalOrdinal)
	}
	return out, nil
}

func crdToAPIRouteSelectionOptions(crd *bgpv1.RouteSelectionOptions) *gobgpapi.RouteSelectionOptionsConfig {
	if crd == nil {
		return nil
	}
	return &gobgpapi.RouteSelectionOptionsConfig{
		AlwaysCompareMed:         crd.AlwaysCompareMed,
		IgnoreAsPathLength:       crd.IgnoreAsPathLength,
		ExternalCompareRouterId:  crd.ExternalCompareRouterID,
		DisableBestPathSelection: crd.DisableBestPathSelection,
		// advertiseInactiveRoutes, enableAigp and ignoreNextHopIgpMetric are
		// deliberately not sent: gobgp-netlink v1.3.5 deleted all three because
		// nothing in the daemon implemented them. The CRD fields are retained
		// and inert - see reportInertFields.
	}
}

func crdToAPIConfederation(crd *bgpv1.Confederation) *gobgpapi.Confederation {
	if crd == nil {
		return nil
	}
	return &gobgpapi.Confederation{
		Enabled:      crd.Enabled,
		Identifier:   crd.Identifier,
		MemberAsList: crd.MemberAsList,
	}
}

func crdToAPIGracefulRestart(crd *bgpv1.GracefulRestart) *gobgpapi.GracefulRestart {
	if crd == nil {
		return nil
	}
	return &gobgpapi.GracefulRestart{
		Enabled:         crd.Enabled,
		RestartTime:     crd.RestartTime,
		StaleRoutesTime: crd.StaleRoutesTime,
		HelperOnly:      crd.HelperOnly,
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
		Type: crdToAPIMatchSetType(crd.Match),
	}
}

// crdToAPIMatchSetType maps the CRD's match strings to the proto enum.
//
// The zero value is TYPE_UNSPECIFIED, and this used to return it for every
// input because the conversion was never written - so a policy saying
// match: "all" was sent as unspecified and matched as though it said "any".
// An empty string keeps that default deliberately; the CRD enum rejects
// anything that is not one of these three.
func crdToAPIMatchSetType(s string) gobgpapi.MatchSet_Type {
	switch s {
	case "all":
		return gobgpapi.MatchSet_TYPE_ALL
	case "invert":
		return gobgpapi.MatchSet_TYPE_INVERT
	case "any":
		return gobgpapi.MatchSet_TYPE_ANY
	default:
		return gobgpapi.MatchSet_TYPE_ANY
	}
}

// crdToAPIPrefixLimit converts a per-afi-safi prefix limit.
//
// family is the afi-safi the limit belongs to: the proto carries it inside the
// limit as well as on the entry, and gobgpd keys the limit off the inner one.
func crdToAPIPrefixLimit(crd *bgpv1.PrefixLimit, family string) *gobgpapi.PrefixLimit {
	if crd == nil {
		return nil
	}
	return &gobgpapi.PrefixLimit{
		Family:               crdToAPIFamily(family),
		MaxPrefixes:          crd.MaxPrefixes,
		ShutdownThresholdPct: crd.ShutdownThresholdPct,
	}
}

func crdToAPIAddPaths(crd *bgpv1.AddPaths) *gobgpapi.AddPaths {
	if crd == nil {
		return nil
	}
	return &gobgpapi.AddPaths{
		Config: &gobgpapi.AddPathsConfig{
			Receive: crd.Receive,
			SendMax: crd.SendMax,
		},
	}
}

// sendCommunityOrdinals maps the CRD's names to oc.CommunityTypeToIntMap.
//
// Read from the fork at the pinned commit, pkg/config/oc/bgp_configs.go - not
// guessed. Note 0 is STANDARD, not none, which is exactly why the proto needed
// explicit presence: under implicit presence an unset field and "standard" were
// the same value on the wire.
var sendCommunityOrdinals = map[string]uint32{
	"standard": 0,
	"extended": 1,
	"both":     2,
	"none":     3,
}

// crdToAPISendCommunity converts the CRD name to the optional proto field.
//
// nil means "not configured", which gobgpd's SendCommunityFromAPI turns into
// the empty CommunityType and leaves its behavior alone. That distinction is
// the whole point of the field being *uint32 as of gobgp-netlink v1.3.1: a bare
// 0 would mean "standard" and would silently start filtering communities on
// every peer that never asked for it.
// Returning nil for an unrecognized name is also what keeps the comparator
// honest. Verified against a live v1.3.1: sending an out-of-range value (99)
// comes back as nil, because SendCommunityFromAPI maps anything outside the
// enum to the empty CommunityType and SendCommunityToAPI renders that as
// absent. Sending one would therefore compare unequal forever. The CRD enum
// makes that unreachable; this fallback is the second line of defense.
func crdToAPISendCommunity(s string) *uint32 {
	v, ok := sendCommunityOrdinals[s]
	if !ok {
		return nil
	}
	return &v
}

// sendCommunityEqual compares the optional field.
//
// Unlike v1.3.0, this round-trips: SendCommunityToAPI returns nil for an unset
// CommunityType rather than a fabricated 0, so ListPeer reports genuine absence
// and nil-vs-nil compares equal instead of churning.
//
// A nil desired means "not stated", which for a neighbor in a peer group is what
// lets the group's value apply. gobgpd then reports the group's resolved value,
// so requiring both sides to be nil would be false for every grouped member that
// inherits - caught by TestRoundTripPeerGroupInheritance against a live v1.3.5,
// where a group setting sendCommunity churned every member that did not.
//
// Skipping a nil desired costs the ability to detect a community filter set
// outside the CR, which is the same trade every other presence-guarded field
// makes here.
func sendCommunityEqual(desired, current *uint32) bool {
	if desired == nil {
		return true
	}
	return current != nil && *desired == *current
}

// ptrIfSet returns &v, or nil when v is the zero value.
//
// This is the send-path half of peer-group inheritance. gobgp-netlink v1.3.5
// gave the inheritable api.PeerConf scalars explicit presence, and treats a
// non-nil field as "the client owns this, do not inherit it"
// (recordNeighborPresence). Sending an explicit zero for a field the CR left
// unset would therefore suppress the peer group's value silently - no churn, no
// error, the group's setting simply stops applying.
//
// The predicate is a zero test, not a presence test, because the CRD fields are
// plain values with omitempty: the apiserver cannot tell us "unset" from
// "explicitly empty or false". The consequence is that a grouped member cannot
// override a group's true with an explicit false, or opt out of a group's
// TCP-MD5 password. Making those CRD fields pointers is the fix; see the field
// comments in api/v1/types.go.
func ptrIfSet[T comparable](v T) *T {
	var zero T
	if v == zero {
		return nil
	}
	return &v
}

// ptrDeref returns *p, or the zero value when p is nil. The inverse of ptrIfSet,
// for the paths that genuinely have no presence to carry.
func ptrDeref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

// crdToAPIRemovePrivatePtr is the api.PeerConf form of crdToAPIRemovePrivate.
// Peers carry explicit presence for this field, so an unset CRD value must send
// nil rather than REMOVE_PRIVATE_UNSPECIFIED - the latter would claim the field
// and stop the peer group supplying it. Peer groups keep the plain form: a group
// is only ever a source of values and never inherits.
func crdToAPIRemovePrivatePtr(s string) *gobgpapi.RemovePrivate {
	v := crdToAPIRemovePrivate(s)
	if v == gobgpapi.RemovePrivate_REMOVE_PRIVATE_UNSPECIFIED {
		return nil
	}
	return &v
}

func crdToAPIRemovePrivate(s string) gobgpapi.RemovePrivate {
	switch s {
	case "all":
		return gobgpapi.RemovePrivate_REMOVE_PRIVATE_ALL
	case "replace":
		return gobgpapi.RemovePrivate_REMOVE_PRIVATE_REPLACE
	default:
		return gobgpapi.RemovePrivate_REMOVE_PRIVATE_UNSPECIFIED
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

// crdToAPICommunityActionType maps the CRD's action type to the proto enum.
//
// This was never written: the converter set only Communities, so every
// community action - add, remove, replace alike - went to gobgpd as
// TYPE_UNSPECIFIED. A policy written to strip a community was not stripping it.
//
// "add" is the fallback rather than UNSPECIFIED because the CRD marks type as
// required and the enum below rejects anything else, so the default is
// unreachable from a valid CR; it exists so the zero value is at least the
// least destructive action.
func crdToAPICommunityActionType(s string) gobgpapi.CommunityAction_Type {
	switch s {
	case "remove":
		return gobgpapi.CommunityAction_TYPE_REMOVE
	case "replace":
		return gobgpapi.CommunityAction_TYPE_REPLACE
	default:
		return gobgpapi.CommunityAction_TYPE_ADD
	}
}

// crdToAPIMedActionType maps "mod"/"replace". Same omission as the community
// action above: the type never reached gobgpd at all.
//
// The distinction matters - "mod" adjusts the MED relative to its current
// value, "replace" overwrites it - so sending neither is not a smaller version
// of either.
func crdToAPIMedActionType(s string) gobgpapi.MedAction_Type {
	switch s {
	case "replace":
		return gobgpapi.MedAction_TYPE_REPLACE
	default:
		return gobgpapi.MedAction_TYPE_MOD
	}
}

// crdToAPIComparison maps eq/ge/le for the length and count conditions.
func crdToAPIComparison(s string) gobgpapi.Comparison {
	switch s {
	case "ge":
		return gobgpapi.Comparison_COMPARISON_GE
	case "le":
		return gobgpapi.Comparison_COMPARISON_LE
	default:
		return gobgpapi.Comparison_COMPARISON_EQ
	}
}

func crdToAPIOriginType(s string) gobgpapi.OriginType {
	switch s {
	case "igp":
		return gobgpapi.OriginType_ORIGIN_TYPE_IGP
	case "egp":
		return gobgpapi.OriginType_ORIGIN_TYPE_EGP
	case "incomplete":
		return gobgpapi.OriginType_ORIGIN_TYPE_INCOMPLETE
	default:
		return gobgpapi.OriginType_ORIGIN_TYPE_UNSPECIFIED
	}
}

func crdToAPIRouteType(s string) gobgpapi.Conditions_RouteType {
	switch s {
	case "internal":
		return gobgpapi.Conditions_ROUTE_TYPE_INTERNAL
	case "external":
		return gobgpapi.Conditions_ROUTE_TYPE_EXTERNAL
	case "local":
		return gobgpapi.Conditions_ROUTE_TYPE_LOCAL
	default:
		return gobgpapi.Conditions_ROUTE_TYPE_UNSPECIFIED
	}
}

func crdToAPIAsPathLength(crd *bgpv1.AsPathLengthCondition) *gobgpapi.AsPathLength {
	if crd == nil {
		return nil
	}
	return &gobgpapi.AsPathLength{Type: crdToAPIComparison(crd.Operator), Length: crd.Length}
}

func crdToAPICommunityCount(crd *bgpv1.CommunityCountCondition) *gobgpapi.CommunityCount {
	if crd == nil {
		return nil
	}
	return &gobgpapi.CommunityCount{Type: crdToAPIComparison(crd.Operator), Count: crd.Count}
}

func crdToAPINexthopAction(crd *bgpv1.NexthopAction) *gobgpapi.NexthopAction {
	if crd == nil {
		return nil
	}
	return &gobgpapi.NexthopAction{
		Address:     crd.Address,
		Self:        crd.Self,
		Unchanged:   crd.Unchanged,
		PeerAddress: crd.PeerAddress,
	}
}

func crdToAPIFamilies(names []string) []*gobgpapi.Family {
	if len(names) == 0 {
		return nil
	}
	out := make([]*gobgpapi.Family, 0, len(names))
	for _, n := range names {
		out = append(out, crdToAPIFamily(n))
	}
	return out
}

func crdToAPICommunityAction(crd *bgpv1.CommunityAction) *gobgpapi.CommunityAction {
	if crd == nil {
		return nil
	}
	return &gobgpapi.CommunityAction{
		Communities: crd.Communities,
		Type:        crdToAPICommunityActionType(crd.Type),
	}
}

func crdToAPIMedAction(crd *bgpv1.MedAction) *gobgpapi.MedAction {
	if crd == nil {
		return nil
	}
	return &gobgpapi.MedAction{
		Value: crd.Value,
		Type:  crdToAPIMedActionType(crd.Type),
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

// crdToAPIFamily converts one family name.
//
// It used to know three families and return AFI_UNSPECIFIED/SAFI_UNSPECIFIED
// for everything else, so afiSafis[].family: "ipv4-vpn" was accepted by the CRD
// and sent to gobgpd as no family at all. Unknown names still fall back rather
// than erroring, because the CRD enum now rejects them before they get here,
// but the fallback is logged-by-construction: an unspecified family is visibly
// wrong in `gobgp neighbor`, where a silently substituted one would not be.
func crdToAPIFamily(s string) *gobgpapi.Family {
	if f, ok := bgpFamilies[strings.ToLower(s)]; ok {
		return &gobgpapi.Family{Afi: f.afi, Safi: f.safi}
	}
	return &gobgpapi.Family{Afi: gobgpapi.Family_AFI_UNSPECIFIED, Safi: gobgpapi.Family_SAFI_UNSPECIFIED}
}

func (r *BGPConfigurationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Initialize node cache for router ID resolution
	r.InitNodeCache()

	return ctrl.NewControllerManagedBy(mgr).
		For(&bgpv1.BGPConfiguration{}).
		Watches(&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(r.nodeToRequests),
			builder.WithPredicates(r.nodeLabelChangePredicate()),
		).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 1,
			RateLimiter:             workqueue.NewTypedItemExponentialFailureRateLimiter[ctrl.Request](time.Second, 5*time.Minute),
		}).
		Complete(r)
}

// nodeLabelChangePredicate returns a predicate that only passes events for this
// pod's node when labels change.
func (r *BGPConfigurationReconciler) nodeLabelChangePredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return false
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			if e.ObjectNew.GetName() != r.NodeName {
				return false
			}
			oldLabels := e.ObjectOld.GetLabels()
			newLabels := e.ObjectNew.GetLabels()
			if !reflect.DeepEqual(oldLabels, newLabels) {
				// Invalidate cached node so reconciler reads fresh labels
				if r.nodeCache != nil {
					r.nodeCache.invalidate(r.NodeName)
				}
				return true
			}
			return false
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return false
		},
	}
}

// nodeToRequests maps a Node event to reconcile requests for all BGPConfigurations.
func (r *BGPConfigurationReconciler) nodeToRequests(ctx context.Context, obj client.Object) []reconcile.Request {
	var configs bgpv1.BGPConfigurationList
	if err := r.List(ctx, &configs); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for _, cfg := range configs.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      cfg.Name,
				Namespace: cfg.Namespace,
			},
		})
	}
	return requests
}
