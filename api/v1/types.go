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

package v1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:object:generate=true
// +kubebuilder:resource:shortName=bgpconfig,scope=Namespaced,path=configs,singular=config
type BGPConfiguration struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              BGPConfigurationSpec   `json:"spec,omitempty"`
	Status            BGPConfigurationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:generate=true
type BGPConfigurationSpec struct {
	Global GlobalSpec `json:"global"`
	// +kubebuilder:validation:MaxItems=1024
	Neighbors []Neighbor `json:"neighbors,omitempty"`
	// +kubebuilder:validation:MaxItems=256
	PeerGroups []PeerGroup `json:"peerGroups,omitempty"`
	// +kubebuilder:validation:MaxItems=256
	DynamicNeighbors []DynamicNeighbor `json:"dynamicNeighbors,omitempty"`
	// +kubebuilder:validation:MaxItems=256
	Vrfs []Vrf `json:"vrfs,omitempty"`
	// +kubebuilder:validation:MaxItems=256
	PolicyDefinitions []PolicyDefinition `json:"policyDefinitions,omitempty"`
	// +kubebuilder:validation:MaxItems=256
	DefinedSets   []DefinedSet   `json:"definedSets,omitempty"`
	NetlinkImport *NetlinkImport `json:"netlinkImport,omitempty"`
	NetlinkExport *NetlinkExport `json:"netlinkExport,omitempty"`
}

// NetlinkImport configures global netlink import for importing connected routes
// +kubebuilder:object:generate=true
type NetlinkImport struct {
	// Enabled activates netlink import functionality
	Enabled bool `json:"enabled,omitempty"`
	// Vrf specifies the VRF to import routes into (empty = global RIB)
	Vrf string `json:"vrf,omitempty"`
	// InterfaceList specifies interfaces to import routes from (supports glob patterns like "eth*")
	InterfaceList []string `json:"interfaceList,omitempty"`
}

// NetlinkExport configures global netlink export for exporting BGP routes to Linux kernel
// +kubebuilder:object:generate=true
type NetlinkExport struct {
	// Enabled activates netlink export functionality
	Enabled bool `json:"enabled,omitempty"`
	// DampeningInterval in milliseconds to prevent flapping (default: 100)
	DampeningInterval uint32 `json:"dampeningInterval,omitempty"`
	// RouteProtocol is the Linux route protocol identifier (default: 186 = RTPROT_BGP)
	RouteProtocol int32 `json:"routeProtocol,omitempty"`
	// Rules defines export rules for matching and exporting routes
	Rules []NetlinkExportRule `json:"rules,omitempty"`
}

// NetlinkExportRule defines a rule for exporting routes to Linux kernel
// +kubebuilder:object:generate=true
type NetlinkExportRule struct {
	// Name is a unique identifier for this export rule
	Name string `json:"name"`
	// CommunityList filters routes by standard BGP communities (format: "AS:VALUE" where AS and VALUE are 0-65535)
	// +kubebuilder:validation:items:Pattern=`^\d{1,5}:\d{1,5}$`
	// +kubebuilder:validation:MaxItems=128
	CommunityList []string `json:"communityList,omitempty"`
	// LargeCommunityList filters routes by large BGP communities (format: "ASN:LocalData1:LocalData2")
	// +kubebuilder:validation:items:Pattern=`^\d+:\d+:\d+$`
	// +kubebuilder:validation:MaxItems=128
	LargeCommunityList []string `json:"largeCommunityList,omitempty"`
	// Vrf is the target VRF name (empty = global routing table)
	Vrf string `json:"vrf,omitempty"`
	// TableId is the Linux routing table ID (0 = main table)
	TableId int32 `json:"tableId,omitempty"`
	// Metric is the route metric/priority in Linux routing table (default: 20)
	Metric uint32 `json:"metric,omitempty"`
	// ValidateNexthop enables nexthop reachability validation before exporting (default: true)
	ValidateNexthop *bool `json:"validateNexthop,omitempty"`
}

// --- Core Config Sections ---

// +kubebuilder:object:generate=true
type GlobalSpec struct {
	// ASN is the Autonomous System Number for this BGP speaker
	ASN uint32 `json:"asn"`
	// RouterID is the BGP router identifier. Must be a valid IPv4 address.
	// Supports three resolution modes:
	// 1. Explicit IPv4: "192.168.1.1" - used as-is
	// 2. Template variable: "${NODE_IP}", "${NODE_IPV4}", "${NODE_EXTERNAL_IP}",
	//    or "${node.annotations['key']}" - resolved from node
	// 3. Auto-detect: when omitted, auto-detects from node's internal IPv4,
	//    or generates from node name hash if no IPv4 available
	// +optional
	RouterID string `json:"routerID,omitempty"`
	// RouterIDPool is the CIDR pool for hash-based router ID generation.
	// Used when RouterID is omitted and node has no IPv4 address (IPv6-only nodes).
	// Must be at least /24 (256 addresses). Each cluster should use a unique pool.
	// Default: "10.255.0.0/16"
	// +optional
	RouterIDPool string `json:"routerIDPool,omitempty"`
	ListenPort   int32  `json:"listenPort,omitempty"`
	// ListenAddresses are the local addresses gobgpd accepts BGP connections on.
	// Omit it to take gobgpd's default, which is every interface ("0.0.0.0" and
	// "::").
	//
	// The pattern rejects obvious nonsense early, with a message that names the
	// field. It is deliberately not a complete IP grammar - the authoritative
	// check is netip.ParseAddr in the controller, which is what gobgpd itself
	// uses, so a value that passes here and fails there is reported before
	// StartBgp rather than after.
	//
	// CEL isIP() would be stricter but needs apiserver 1.31+, and a CEL rule that
	// cannot compile makes the whole CRD unapplyable rather than degrading.
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=45
	// +kubebuilder:validation:items:Pattern=`^[0-9a-fA-F:.]+(%[0-9a-zA-Z._-]+)?$`
	// +optional
	ListenAddresses []string `json:"listenAddresses,omitempty"`
	// Families is the global address-family set. Omit it to keep gobgpd's
	// default of ipv4-unicast and ipv6-unicast.
	//
	// The enum is enforced because the wire encoding is a bare ordinal: an
	// unrecognized name cannot be passed through, and a wrong value does not
	// error - it enables no families at all. See globalFamilyOrdinals.
	// +kubebuilder:validation:items:Enum=ipv4-unicast;ipv6-unicast;ipv4-labeled;ipv6-labeled;ipv4-vpn;ipv6-vpn;l2vpn-vpls;l2vpn-evpn
	// +kubebuilder:validation:MaxItems=16
	// +optional
	Families              []string               `json:"families,omitempty"`
	UseMultiplePaths      bool                   `json:"useMultiplePaths,omitempty"`
	RouteSelectionOptions *RouteSelectionOptions `json:"routeSelectionOptions,omitempty"`
	// INERT as of gobgp-netlink v1.3.5, which removed the field from its API
	// because nothing in the daemon implemented it. Accepted here so existing
	// manifests keep applying; setting it has no effect and logs a warning event.
	DefaultRouteDistance *DefaultRouteDistance `json:"defaultRouteDistance,omitempty"`
	Confederation        *Confederation        `json:"confederation,omitempty"`
	GracefulRestart      *GracefulRestart      `json:"gracefulRestart,omitempty"`
	ApplyPolicy          *ApplyPolicy          `json:"applyPolicy,omitempty"`
	BindToDevice         string                `json:"bindToDevice,omitempty"`
	// NodeStatus configures per-node BGPNodeStatus reporting
	// +optional
	NodeStatus *NodeStatusConfig `json:"nodeStatus,omitempty"`
}

// BFD configures Bidirectional Forwarding Detection for a session.
//
// Inheritance is whole-block, not per-field: a neighbor with no bfd block takes
// its peer group's, and a neighbor with one overrides the group entirely. Since
// the defaults below are applied by the API server as soon as a bfd block
// exists, that override is always total - which is what makes
// `bfd: {enabled: false}` a working opt-out from a group that has BFD on.
//
// Timers are microseconds. The minimums are gobgpd's, not ours: a faster
// profile is rejected at apply time rather than silently accepted. Detection
// time is requiredMinimumReceive x detectionMultiplier, so the defaults below
// give 3s.
//
// Note that a BFD transition is a *hard* BGP reset, and that changing BFD
// config tears the session down and rebuilds it - which stops our transmit, so
// the remote end's detect timer expires and it resets too. Apply BFD changes in
// a maintenance window, and see README.md on why sub-second profiles are not
// viable under the DaemonSet's default CPU limit.
// +kubebuilder:object:generate=true
type BFD struct {
	// Enabled activates BFD for this session.
	//
	// A pointer so that `enabled: false` survives serialization: as a plain
	// bool with omitempty it would be indistinguishable from an absent block,
	// and an absent block means "inherit from the peer group".
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Port is the destination UDP port for control packets sent to this peer.
	//
	// The local listener is always 3784 regardless of this value, so setting
	// anything else means transmitting to a port the peer is probably not
	// listening on. RFC 5881 mandates 3784; there is rarely a reason to change
	// it.
	// +kubebuilder:validation:Minimum=1024
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=3784
	// +optional
	Port uint32 `json:"port,omitempty"`

	// DesiredMinimumTxInterval is the minimum interval, in MICROSECONDS,
	// between transmitted control packets. 300000 is 300ms; 300 is 300
	// microseconds.
	// +kubebuilder:validation:Minimum=300000
	// +kubebuilder:validation:Maximum=30000000
	// +kubebuilder:default=1000000
	// +optional
	DesiredMinimumTxInterval uint32 `json:"desiredMinimumTxInterval,omitempty"`

	// RequiredMinimumReceive is the minimum interval, in MICROSECONDS, between
	// received control packets.
	// +kubebuilder:validation:Minimum=300000
	// +kubebuilder:validation:Maximum=30000000
	// +kubebuilder:default=1000000
	// +optional
	RequiredMinimumReceive uint32 `json:"requiredMinimumReceive,omitempty"`

	// DetectionMultiplier is how many intervals may be missed before the
	// session is declared down. Detection time is
	// requiredMinimumReceive x detectionMultiplier.
	// +kubebuilder:validation:Minimum=3
	// +kubebuilder:validation:Maximum=255
	// +kubebuilder:default=3
	// +optional
	DetectionMultiplier uint32 `json:"detectionMultiplier,omitempty"`
}

// +kubebuilder:object:generate=true
// Neighbor is one BGP peer.
//
// The BFD/link-local rule below catches a failure with no other symptom: BFD
// control packets to a link-local address need the scope zone to pick an
// egress interface, and without one transmit still succeeds on some arbitrary
// interface. BGP establishes, everything reads healthy, and the BFD session
// never leaves Down - so the failure detector the operator just enabled is not
// running. Use `fe80::1%eth0`, or `neighborInterface`, which resolves the zone
// itself.
//
// +kubebuilder:validation:XValidation:rule="!has(self.bfd) || (has(self.bfd.enabled) && !self.bfd.enabled) || !has(self.config.neighborAddress) || !self.config.neighborAddress.lowerAscii().startsWith('fe80') || self.config.neighborAddress.contains('%')",message="a link-local neighborAddress needs its scope zone when BFD is enabled (fe80::1%eth0, not fe80::1); or use neighborInterface instead"
type Neighbor struct {
	Config          NeighborConfig        `json:"config"`
	NodeSelector    *metav1.LabelSelector `json:"nodeSelector,omitempty"`
	AfiSafis        []AfiSafi             `json:"afiSafis,omitempty"`
	ApplyPolicy     *ApplyPolicy          `json:"applyPolicy,omitempty"`
	Timers          *Timers               `json:"timers,omitempty"`
	Transport       *Transport            `json:"transport,omitempty"`
	GracefulRestart *GracefulRestart      `json:"gracefulRestart,omitempty"`
	RouteReflector  *RouteReflector       `json:"routeReflector,omitempty"`
	EbgpMultihop    *EbgpMultihop         `json:"ebgpMultihop,omitempty"`
	// BFD, if set, overrides whatever the peer group specifies. Omit it to
	// inherit. See the BFD type for why the override is whole-block.
	// +optional
	BFD *BFD `json:"bfd,omitempty"`
}

// +kubebuilder:object:generate=true
type PeerGroup struct {
	Config       PeerGroupConfig       `json:"config"`
	NodeSelector *metav1.LabelSelector `json:"nodeSelector,omitempty"`
	AfiSafis     []AfiSafi             `json:"afiSafis,omitempty"`
	ApplyPolicy  *ApplyPolicy          `json:"applyPolicy,omitempty"`
	Timers       *Timers               `json:"timers,omitempty"`
	Transport    *Transport            `json:"transport,omitempty"`
	// BFD applies to every member that does not specify its own.
	// +optional
	BFD *BFD `json:"bfd,omitempty"`
}

// --- Detailed Sub-structs ---

// NeighborConfig is a neighbor's own settings.
//
// Peer-group inheritance: for a neighbor that names a peerGroup, any field left
// unset here takes the group's value. That is why an omitted field is sent to
// gobgpd as absent rather than as a zero - stating a field claims it and stops
// the group supplying it.
//
// Two consequences worth knowing:
//
//   - A member CAN override a group's value with a zero one. description,
//     authPassword, allowOwnAsn, replacePeerAsn, allowAspathLoopLocal and
//     sendSoftwareVersion carry explicit presence, so `authPassword: ""` opts a
//     member out of its group's TCP-MD5 password and `replacePeerAsn: false`
//     overrides a group that sets it true. Omitting the field inherits; setting
//     it - to anything, including the zero value - claims it.
//
//   - allowOwnAsn, replacePeerAsn and allowAspathLoopLocal share one presence
//     signal in gobgpd. Stating any one of them claims all three, so the other
//     two fall back to their defaults instead of inheriting from the group.
//     State all three together, or none.
//
//   - peerAsn is the exception: where a peer group states one, the group's value
//     always wins and a differing value here is discarded. The controller emits a
//     PeerAsnOverridden warning event when that happens.
//
// +kubebuilder:object:generate=true
type NeighborConfig struct {
	// AuthPassword is the BGP authentication password (DEPRECATED: use AuthPasswordSecretRef instead)
	// +optional
	AuthPassword *string `json:"authPassword,omitempty"`
	// AuthPasswordSecretRef references a Secret containing the BGP authentication password
	// The Secret must contain a key matching the neighbor address or a default key "password"
	// +optional
	AuthPasswordSecretRef *corev1.SecretKeySelector `json:"authPasswordSecretRef,omitempty"`
	Description           *string                   `json:"description,omitempty"`
	LocalAsn              uint32                    `json:"localAsn,omitempty"`
	// NeighborAddress is the peer's IP address. For a link-local IPv6 peer,
	// include the scope zone: fe80::1%eth0.
	//
	// The length bound is not cosmetic. CEL rules are costed statically against
	// the declared maxLength, so an unbounded string here makes the BFD
	// link-local rule on Neighbor exceed the apiserver's per-rule cost budget
	// by ~64x and the CRD is rejected outright. 64 = 45 (the longest textual
	// IPv6 form, ::ffff:255.255.255.255) + '%' + 15 (IFNAMSIZ-1), rounded up.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	NeighborAddress   string `json:"neighborAddress,omitempty"`
	PeerAsn           uint32 `json:"peerAsn"`
	PeerGroup         string `json:"peerGroup,omitempty"`
	AdminDown         bool   `json:"adminDown,omitempty"`
	NeighborInterface string `json:"neighborInterface,omitempty"`
	Vrf               string `json:"vrf,omitempty"`

	// AllowOwnAsn permits our own ASN to appear this many times in a received
	// AS path.
	//
	// The bound is load-bearing, not cosmetic. gobgpd's peer-group path errors
	// above MaxUint8, but its neighbor path does a bare uint8() cast with no
	// check (grpc_server.go, newNeighborFromAPIStruct), so 256 would silently
	// truncate to 0 - the opposite of what was asked for.
	// +kubebuilder:validation:Maximum=255
	// +optional
	AllowOwnAsn *uint32 `json:"allowOwnAsn,omitempty"`
	// ReplacePeerAsn rewrites the peer's ASN with ours in advertised AS paths.
	// +optional
	ReplacePeerAsn *bool `json:"replacePeerAsn,omitempty"`
	// AllowAspathLoopLocal permits a local AS-path loop.
	// +optional
	AllowAspathLoopLocal *bool `json:"allowAspathLoopLocal,omitempty"`
	// SendCommunity filters which community attributes are advertised to this
	// peer, applied AFTER the export policy - so a policy that adds a community
	// cannot walk past it.
	//
	// Dead in every gobgp release before gobgp-netlink v1.3.1: accepted by the
	// config loader, dropped by the gRPC converters, read by nothing. Omit it to
	// leave gobgpd's behavior alone.
	//
	// "none" is not literal. Large communities, LLGR_STALE/NO_LLGR, and every
	// family except IPv4/IPv6 unicast and labeled unicast survive every
	// setting, and it is ignored entirely for route-server clients. See the BGP
	// section of README.md.
	//
	// Note "none" and "extended" strip NO_EXPORT, NO_ADVERTISE and
	// NO_EXPORT_SUBCONFED, so the receiving AS loses the signal not to
	// re-export. That is a route-leak vector, not just a filter.
	// +kubebuilder:validation:Enum=standard;extended;both;none
	// +optional
	SendCommunity string `json:"sendCommunity,omitempty"`
	// RemovePrivate strips private ASNs from advertised AS paths:
	// "all" removes them, "replace" substitutes our own ASN.
	// +kubebuilder:validation:Enum=all;replace
	// +optional
	RemovePrivate string `json:"removePrivate,omitempty"`
	// RouteFlapDamping enables RFC 2439 damping for this peer.
	// INERT as of gobgp-netlink v1.3.5, which removed the field from its API
	// because nothing in the daemon implemented it. Accepted here so existing
	// manifests keep applying; setting it has no effect and logs a warning event.
	// +optional
	RouteFlapDamping bool `json:"routeFlapDamping,omitempty"`
	// SendSoftwareVersion advertises the software-version capability (RFC 9384).
	// +optional
	SendSoftwareVersion *bool `json:"sendSoftwareVersion,omitempty"`
}

// +kubebuilder:object:generate=true
type PeerGroupConfig struct {
	PeerGroupName string `json:"peerGroupName"`
	Description   string `json:"description,omitempty"`
	PeerAsn       uint32 `json:"peerAsn,omitempty"`
	LocalAsn      uint32 `json:"localAsn,omitempty"`
	// AuthPassword is the BGP authentication password (DEPRECATED: use AuthPasswordSecretRef instead)
	// +optional
	AuthPassword string `json:"authPassword,omitempty"`
	// AuthPasswordSecretRef references a Secret containing the BGP authentication password
	// +optional
	AuthPasswordSecretRef *corev1.SecretKeySelector `json:"authPasswordSecretRef,omitempty"`

	// AllowOwnAsn permits this many occurrences of our own ASN in a received
	// AS path. Bounded at 255 because gobgpd rejects anything larger.
	// +kubebuilder:validation:Maximum=255
	// +optional
	AllowOwnAsn uint32 `json:"allowOwnAsn,omitempty"`
	// ReplacePeerAsn replaces the peer's ASN in the AS path with our own.
	// +optional
	ReplacePeerAsn bool `json:"replacePeerAsn,omitempty"`
	// AllowAspathLoopLocal permits our own ASN in locally originated paths.
	// +optional
	AllowAspathLoopLocal bool `json:"allowAspathLoopLocal,omitempty"`
	// SendCommunity filters which community attributes are advertised to this
	// peer, applied AFTER the export policy - so a policy that adds a community
	// cannot walk past it.
	//
	// Dead in every gobgp release before gobgp-netlink v1.3.1: accepted by the
	// config loader, dropped by the gRPC converters, read by nothing. Omit it to
	// leave gobgpd's behavior alone.
	//
	// "none" is not literal. Large communities, LLGR_STALE/NO_LLGR, and every
	// family except IPv4/IPv6 unicast and labeled unicast survive every
	// setting, and it is ignored entirely for route-server clients. See the BGP
	// section of README.md.
	//
	// Note "none" and "extended" strip NO_EXPORT, NO_ADVERTISE and
	// NO_EXPORT_SUBCONFED, so the receiving AS loses the signal not to
	// re-export. That is a route-leak vector, not just a filter.
	// +kubebuilder:validation:Enum=standard;extended;both;none
	// +optional
	SendCommunity string `json:"sendCommunity,omitempty"`
	// RemovePrivate strips private ASNs from advertised AS paths:
	// "all" removes them, "replace" substitutes our own ASN.
	// +kubebuilder:validation:Enum=all;replace
	// +optional
	RemovePrivate string `json:"removePrivate,omitempty"`
	// RouteFlapDamping enables RFC 2439 damping for members of this group.
	// INERT as of gobgp-netlink v1.3.5, which removed the field from its API
	// because nothing in the daemon implemented it. Accepted here so existing
	// manifests keep applying; setting it has no effect and logs a warning event.
	// +optional
	RouteFlapDamping bool `json:"routeFlapDamping,omitempty"`
	// SendSoftwareVersion advertises the software-version capability (RFC 9384).
	// +optional
	SendSoftwareVersion bool `json:"sendSoftwareVersion,omitempty"`
}

// +kubebuilder:object:generate=true
type AfiSafi struct {
	// Family is the address family, e.g. ipv4-unicast.
	//
	// Enforced as an enum because the converter previously recognized only
	// three names and sent AFI_UNSPECIFIED for anything else - so a typo, or a
	// perfectly valid family it did not know, was accepted by the CRD and
	// silently produced a peer with no usable family.
	// +kubebuilder:validation:Enum=ipv4-unicast;ipv6-unicast;ipv4-labeled;ipv6-labeled;ipv4-vpn;ipv6-vpn;l2vpn-vpls;l2vpn-evpn;ipv4-flowspec;ipv6-flowspec;rtc
	Family      string       `json:"family"` // e.g., "ipv4-unicast", "l2vpn-evpn"
	Enabled     bool         `json:"enabled"`
	PrefixLimit *PrefixLimit `json:"prefixLimit,omitempty"`
	AddPaths    *AddPaths    `json:"addPaths,omitempty"`
}

// +kubebuilder:object:generate=true
type PrefixLimit struct {
	MaxPrefixes          uint32 `json:"maxPrefixes"`
	ShutdownThresholdPct uint32 `json:"shutdownThresholdPct,omitempty"`
}

// +kubebuilder:object:generate=true
type AddPaths struct {
	Receive bool   `json:"receive,omitempty"`
	SendMax uint32 `json:"sendMax,omitempty"`
}

// +kubebuilder:object:generate=true
// Timers holds session timers.
//
// Whole-block inheritance: for a neighbor in a peer group, supplying this block
// at all claims every timer in it. A neighbor that sets only connectRetry
// therefore stops inheriting the group's holdTime and keepaliveInterval and falls
// back to gobgpd's defaults. Repeat the group's values here, or omit the block.
//
// Editing holdTime or keepaliveInterval rebuilds the BGP session as of
// gobgp-netlink v1.3.5; connectRetry and idleHoldTimeAfterReset do not. Editing
// them on a peer group rebuilds every member's session at once, so enable
// graceful restart first.
type Timers struct {
	Config TimersConfig `json:"config"`
}

// +kubebuilder:object:generate=true
type TimersConfig struct {
	ConnectRetry      uint64 `json:"connectRetry,omitempty"`
	HoldTime          uint64 `json:"holdTime,omitempty"`
	KeepaliveInterval uint64 `json:"keepaliveInterval,omitempty"`
	// INERT as of gobgp-netlink v1.3.5, which removed the field from its API
	// because nothing in the daemon implemented it. Accepted here so existing
	// manifests keep applying; setting it has no effect and logs a warning event.
	MinimumAdvertisementInterval uint64 `json:"minimumAdvertisementInterval,omitempty"`
}

// +kubebuilder:object:generate=true
type Transport struct {
	LocalAddress  string `json:"localAddress,omitempty"`
	PassiveMode   bool   `json:"passiveMode,omitempty"`
	BindInterface string `json:"bindInterface,omitempty"`
	// IpTos sets IP_TOS / IPV6_TCLASS on the BGP session.
	//
	// Bounded at 255 because it is a single IP header byte and gobgpd casts it
	// to uint8 with no range check - an unbounded value would be silently
	// truncated rather than rejected.
	// +kubebuilder:validation:Maximum=255
	// +optional
	IpTos uint32 `json:"ipTos,omitempty"`
}

// +kubebuilder:object:generate=true
type GracefulRestart struct {
	Enabled bool `json:"enabled,omitempty"`
	// RestartTime is how long a peer should hold this speaker's routes while it
	// restarts, in seconds. Omit it to take gobgpd's default, which is the
	// session's hold time.
	//
	// Bounded at 4095 because RFC 4724 section 3 gives Restart Time 12 bits,
	// sharing a 16-bit word with a 4-bit flags nibble. A larger value does not
	// fail - it silently overflows into the flags and corrupts the capability, so
	// this is not a Go type limit and 65535 would be the wrong bound.
	// +kubebuilder:validation:Maximum=4095
	// +optional
	RestartTime uint32 `json:"restartTime,omitempty"`
	HelperOnly  bool   `json:"helperOnly,omitempty"`
}

// +kubebuilder:object:generate=true
type RouteReflector struct {
	RouteReflectorClient    bool   `json:"routeReflectorClient"`
	RouteReflectorClusterId string `json:"routeReflectorClusterId"`
}

// +kubebuilder:object:generate=true
type EbgpMultihop struct {
	Enabled     bool   `json:"enabled"`
	MultihopTtl uint32 `json:"multihopTtl,omitempty"`
}

// +kubebuilder:object:generate=true
type DynamicNeighbor struct {
	Prefix    string `json:"prefix"`
	PeerGroup string `json:"peerGroup"`
}

// +kubebuilder:object:generate=true
type Vrf struct {
	Name          string            `json:"name"`
	Rd            string            `json:"rd,omitempty"`
	ImportRt      []string          `json:"importRt,omitempty"`
	ExportRt      []string          `json:"exportRt,omitempty"`
	NetlinkImport *VrfNetlinkImport `json:"netlinkImport,omitempty"`
	NetlinkExport *VrfNetlinkExport `json:"netlinkExport,omitempty"`
}

// VrfNetlinkImport configures netlink import for a specific VRF
// +kubebuilder:object:generate=true
type VrfNetlinkImport struct {
	// Enabled activates netlink import for this VRF
	Enabled bool `json:"enabled,omitempty"`
	// InterfaceList specifies interfaces to import routes from (supports glob patterns)
	InterfaceList []string `json:"interfaceList,omitempty"`
}

// VrfNetlinkExport configures netlink export for a specific VRF
// +kubebuilder:object:generate=true
type VrfNetlinkExport struct {
	// Enabled activates VRF-to-VRF export
	Enabled bool `json:"enabled,omitempty"`
	// LinuxVrf is the target Linux VRF name (defaults to GoBGP VRF name)
	LinuxVrf string `json:"linuxVrf,omitempty"`
	// LinuxTableId is the target Linux routing table ID (auto-lookup from Linux VRF if not specified)
	LinuxTableId int32 `json:"linuxTableId,omitempty"`
	// Metric is the route metric/priority in Linux routing table
	Metric uint32 `json:"metric,omitempty"`
	// ValidateNexthop enables nexthop reachability validation before exporting (default: true)
	ValidateNexthop *bool `json:"validateNexthop,omitempty"`
	// CommunityList filters routes by standard BGP communities (format: "AS:VALUE" where AS and VALUE are 0-65535)
	// +kubebuilder:validation:items:Pattern=`^\d{1,5}:\d{1,5}$`
	// +kubebuilder:validation:MaxItems=128
	CommunityList []string `json:"communityList,omitempty"`
	// LargeCommunityList filters routes by large BGP communities (format: "ASN:LocalData1:LocalData2")
	// +kubebuilder:validation:items:Pattern=`^\d+:\d+:\d+$`
	// +kubebuilder:validation:MaxItems=128
	LargeCommunityList []string `json:"largeCommunityList,omitempty"`
}

// +kubebuilder:object:generate=true
type RouteSelectionOptions struct {
	// AlwaysCompareMed compares MED across paths from different ASes.
	AlwaysCompareMed bool `json:"alwaysCompareMed,omitempty"`
	// IgnoreAsPathLength skips AS-path length in best-path selection.
	IgnoreAsPathLength bool `json:"ignoreAsPathLength,omitempty"`
	// ExternalCompareRouterID compares router IDs for external paths.
	ExternalCompareRouterID bool `json:"externalCompareRouterID,omitempty"`
	// AdvertiseInactiveRoutes advertises routes not installed in the FIB.
	// INERT as of gobgp-netlink v1.3.5, which removed the field from its API
	// because nothing in the daemon implemented it. Accepted here so existing
	// manifests keep applying; setting it has no effect and logs a warning event.
	AdvertiseInactiveRoutes bool `json:"advertiseInactiveRoutes,omitempty"`
	// EnableAigp enables AIGP metric comparison.
	// INERT as of gobgp-netlink v1.3.5, which removed the field from its API
	// because nothing in the daemon implemented it. Accepted here so existing
	// manifests keep applying; setting it has no effect and logs a warning event.
	EnableAigp bool `json:"enableAigp,omitempty"`
	// IgnoreNextHopIgpMetric skips the next-hop IGP metric.
	// INERT as of gobgp-netlink v1.3.5, which removed the field from its API
	// because nothing in the daemon implemented it. Accepted here so existing
	// manifests keep applying; setting it has no effect and logs a warning event.
	IgnoreNextHopIgpMetric bool `json:"ignoreNextHopIgpMetric,omitempty"`
	// DisableBestPathSelection turns off best-path selection entirely.
	DisableBestPathSelection bool `json:"disableBestPathSelection,omitempty"`
}

// +kubebuilder:object:generate=true
type DefaultRouteDistance struct {
	ExternalRouteDistance uint32 `json:"externalRouteDistance,omitempty"`
	InternalRouteDistance uint32 `json:"internalRouteDistance,omitempty"`
}

// +kubebuilder:object:generate=true
type Confederation struct {
	Enabled      bool     `json:"enabled,omitempty"`
	Identifier   uint32   `json:"identifier,omitempty"`
	MemberAsList []uint32 `json:"memberAsList,omitempty"`
}

// --- Policy and DefinedSets ---

// +kubebuilder:object:generate=true
type ApplyPolicy struct {
	ExportPolicy *PolicyAssignment `json:"exportPolicy,omitempty"`
	ImportPolicy *PolicyAssignment `json:"importPolicy,omitempty"`
}

// +kubebuilder:object:generate=true
type PolicyAssignment struct {
	Name          string   `json:"name,omitempty"`
	Policies      []string `json:"policies,omitempty"`
	DefaultAction string   `json:"defaultAction,omitempty"` // "accept" or "reject"
}

// +kubebuilder:object:generate=true
type PolicyDefinition struct {
	Name string `json:"name"`
	// Statements are evaluated in order.
	//
	// Bounded because CEL rules nested below here are costed statically
	// against this bound; unbounded, the estimator assumes the maximum and
	// pushed the whole CRD over the apiserver budget, rejecting it outright.
	// +kubebuilder:validation:MaxItems=128
	Statements []Statement `json:"statements"`
}

// +kubebuilder:object:generate=true
type Statement struct {
	Name       string     `json:"name"`
	Conditions Conditions `json:"conditions"`
	Actions    Actions    `json:"actions"`
}

// +kubebuilder:object:generate=true
type Conditions struct {
	PrefixSet    *MatchSet `json:"prefixSet,omitempty"`
	NeighborSet  *MatchSet `json:"neighborSet,omitempty"`
	AsPathSet    *MatchSet `json:"asPathSet,omitempty"`
	CommunitySet *MatchSet `json:"communitySet,omitempty"`
	RpkiResult   string    `json:"rpkiResult,omitempty"` // "valid", "invalid", "not-found"

	// ExtCommunitySet matches extended communities.
	// +optional
	ExtCommunitySet *MatchSet `json:"extCommunitySet,omitempty"`
	// LargeCommunitySet matches large communities. Note NetlinkExportRule
	// already supported large communities; BGP policy did not.
	// +optional
	LargeCommunitySet *MatchSet `json:"largeCommunitySet,omitempty"`
	// AsPathLength matches on the number of ASes in the AS path.
	// +optional
	AsPathLength *AsPathLengthCondition `json:"asPathLength,omitempty"`
	// CommunityCount matches on how many communities a route carries.
	// +optional
	CommunityCount *CommunityCountCondition `json:"communityCount,omitempty"`
	// RouteType matches the route's origin relative to this speaker.
	// +kubebuilder:validation:Enum=internal;external;local
	// +optional
	RouteType string `json:"routeType,omitempty"`
	// Origin matches the ORIGIN attribute.
	// +kubebuilder:validation:Enum=igp;egp;incomplete
	// +optional
	Origin string `json:"origin,omitempty"`
	// NextHopInList matches when the next hop is one of these addresses.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	NextHopInList []string `json:"nextHopInList,omitempty"`
	// AfiSafiIn matches when the route belongs to one of these families.
	// +kubebuilder:validation:items:Enum=ipv4-unicast;ipv6-unicast;ipv4-labeled;ipv6-labeled;ipv4-vpn;ipv6-vpn;l2vpn-vpls;l2vpn-evpn;ipv4-flowspec;ipv6-flowspec;rtc
	// +kubebuilder:validation:MaxItems=16
	// +optional
	AfiSafiIn []string `json:"afiSafiIn,omitempty"`
	// LocalPrefEq matches an exact LOCAL_PREF. A pointer because 0 is a valid
	// local preference and must be distinguishable from "not matching on it".
	// +optional
	LocalPrefEq *uint32 `json:"localPrefEq,omitempty"`
	// MedEq matches an exact MED. A pointer for the same reason as LocalPrefEq.
	// +optional
	MedEq *uint32 `json:"medEq,omitempty"`
}

// AsPathLengthCondition matches on AS-path length.
// +kubebuilder:object:generate=true
type AsPathLengthCondition struct {
	// Operator is "eq", "ge" or "le".
	// +kubebuilder:validation:Enum=eq;ge;le
	Operator string `json:"operator"`
	// Length is the number of ASes to compare against.
	Length uint32 `json:"length"`
}

// CommunityCountCondition matches on how many communities a route carries.
// +kubebuilder:object:generate=true
type CommunityCountCondition struct {
	// Operator is "eq", "ge" or "le".
	// +kubebuilder:validation:Enum=eq;ge;le
	Operator string `json:"operator"`
	// Count is the number of communities to compare against.
	Count uint32 `json:"count"`
}

// NexthopAction rewrites a route's next hop.
//
// The four fields are mutually exclusive in practice: set an explicit address,
// or exactly one of the booleans.
// +kubebuilder:object:generate=true
// +kubebuilder:validation:XValidation:rule="[has(self.address), has(self.self) && self.self, has(self.unchanged) && self.unchanged, has(self.peerAddress) && self.peerAddress].filter(x, x).size() <= 1",message="set at most one of address, self, unchanged, peerAddress"
type NexthopAction struct {
	// Address sets an explicit next hop.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	Address string `json:"address,omitempty"`
	// Self rewrites the next hop to this speaker's address.
	// +optional
	Self bool `json:"self,omitempty"`
	// Unchanged preserves the received next hop.
	// +optional
	Unchanged bool `json:"unchanged,omitempty"`
	// PeerAddress rewrites the next hop to the peer's address.
	// +optional
	PeerAddress bool `json:"peerAddress,omitempty"`
}

// MatchSet references a DefinedSet and says how its members must match.
//
// match was previously accepted and dropped: crdToAPIMatchSet set only Name, so
// "all" and "invert" were both sent as TYPE_UNSPECIFIED and the policy matched
// differently than it read. That is a silent change of routing policy, not an
// inert no-op, which is why the values are now an enforced enum.
// +kubebuilder:object:generate=true
type MatchSet struct {
	// Name of the DefinedSet to match against.
	Name string `json:"name"`
	// Match is "any" (default), "all", or "invert".
	// +kubebuilder:validation:Enum=any;all;invert
	// +optional
	Match string `json:"match,omitempty"`
}

// +kubebuilder:object:generate=true
type Actions struct {
	RouteAction string           `json:"routeAction"` // "accept" or "reject"
	Community   *CommunityAction `json:"community,omitempty"`
	// ExtCommunity manipulates extended communities.
	// +optional
	ExtCommunity *CommunityAction `json:"extCommunity,omitempty"`
	// LargeCommunity manipulates large communities.
	// +optional
	LargeCommunity *CommunityAction `json:"largeCommunity,omitempty"`
	// Nexthop rewrites the route's next hop.
	// +optional
	Nexthop *NexthopAction `json:"nexthop,omitempty"`
	// Origin sets the ORIGIN attribute.
	// +kubebuilder:validation:Enum=igp;egp;incomplete
	// +optional
	Origin    string           `json:"origin,omitempty"`
	Med       *MedAction       `json:"med,omitempty"`
	AsPrepend *AsPrependAction `json:"asPrepend,omitempty"`
	LocalPref uint32           `json:"localPref,omitempty"`
}

// +kubebuilder:object:generate=true
type CommunityAction struct {
	// Type is "add", "remove" or "replace".
	// +kubebuilder:validation:Enum=add;remove;replace
	Type string `json:"type"`
	// Communities are the values to act on.
	// +kubebuilder:validation:MaxItems=64
	Communities []string `json:"communities"`
}

// +kubebuilder:object:generate=true
type MedAction struct {
	// Type is "mod" (adjust relative to the current MED) or "replace".
	// +kubebuilder:validation:Enum=mod;replace
	Type string `json:"type"`
	// Value is the amount to add, or the value to set.
	Value int64 `json:"value"`
}

// +kubebuilder:object:generate=true
type AsPrependAction struct {
	Asn    uint32 `json:"asn"`
	Repeat uint32 `json:"repeat,omitempty"`
}

// +kubebuilder:object:generate=true
type DefinedSet struct {
	Type string   `json:"type"` // "prefix", "neighbor", "as-path", "community"
	Name string   `json:"name"`
	List []string `json:"list,omitempty"`
	// +kubebuilder:validation:MaxItems=1024
	Prefixes []Prefix `json:"prefixes,omitempty"`
}

// Prefix matches either an IP prefix or a Route Target Constrain prefix -
// exactly one of them. gobgpd rejects a prefix with both set, so ipPrefix is
// optional rather than required; without that, rtcPrefix could never be used.
// +kubebuilder:object:generate=true
// +kubebuilder:validation:XValidation:rule="has(self.ipPrefix) != has(self.rtcPrefix)",message="exactly one of ipPrefix or rtcPrefix must be set"
type Prefix struct {
	// +optional
	IpPrefix string `json:"ipPrefix,omitempty"`
	// RtcPrefix is a Route Target Constrain prefix, for RTC policy matching.
	// +optional
	RtcPrefix     string `json:"rtcPrefix,omitempty"`
	MaskLengthMin uint32 `json:"maskLengthMin,omitempty"`
	MaskLengthMax uint32 `json:"maskLengthMax,omitempty"`
}

// --- Status ---

// +kubebuilder:object:generate=true
type BGPConfigurationStatus struct {
	// ObservedGeneration is the most recent generation observed by the controller
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions represent the latest available observations of the BGP configuration state
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// LastReconcileTime is the timestamp of the last reconciliation
	LastReconcileTime *metav1.Time `json:"lastReconcileTime,omitempty"`
	// NeighborCount is the number of configured BGP neighbors
	NeighborCount int `json:"neighborCount,omitempty"`
	// Message provides additional information about the current state
	Message string `json:"message,omitempty"`
	// RouterIDSource indicates how the router ID was resolved:
	// "explicit", "template", "node-ipv4", or "hash-from-node-name"
	RouterIDSource string `json:"routerIDSource,omitempty"`
	// RouterIDResolutionTime is when the router ID was resolved
	RouterIDResolutionTime *metav1.Time `json:"routerIDResolutionTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:object:generate=true
type BGPConfigurationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BGPConfiguration `json:"items"`
}
