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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:object:generate=true
// +kubebuilder:resource:shortName=bgpns,scope=Cluster,path=bgpnodestatuses,singular=bgpnodestatus
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.status.nodeName`
// +kubebuilder:printcolumn:name="RouterID",type=string,JSONPath=`.status.routerID`
// +kubebuilder:printcolumn:name="Healthy",type=boolean,JSONPath=`.status.healthy`
// +kubebuilder:printcolumn:name="Neighbors",type=integer,JSONPath=`.status.neighborCount`
// +kubebuilder:printcolumn:name="LastUpdated",type=date,JSONPath=`.status.lastUpdated`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type BGPNodeStatus struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              BGPNodeStatusSpec `json:"spec,omitempty"`
	Status            BGPNodeStatusData `json:"status,omitempty"`
}

// BGPNodeStatusSpec is intentionally empty — BGPNodeStatus is a status-only resource.
// +kubebuilder:object:generate=true
type BGPNodeStatusSpec struct{}

// +kubebuilder:object:root=true
// +kubebuilder:object:generate=true
type BGPNodeStatusList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BGPNodeStatus `json:"items"`
}

// BGPNodeStatusData holds the per-node BGP state reported by the k8gobgp agent.
// +kubebuilder:object:generate=true
type BGPNodeStatusData struct {
	// NodeName is the Kubernetes node this status belongs to
	NodeName string `json:"nodeName"`
	// RouterID is the resolved BGP router identifier
	RouterID string `json:"routerID,omitempty"`
	// RouterIDSource indicates how the router ID was resolved:
	// "explicit", "template", "node-ipv4", or "hash-from-node-name"
	RouterIDSource string `json:"routerIDSource,omitempty"`
	// ASN is the local Autonomous System Number
	ASN uint32 `json:"asn,omitempty"`
	// LastUpdated is the timestamp of the last status write
	LastUpdated *metav1.Time `json:"lastUpdated,omitempty"`
	// HeartbeatSeconds is the configured heartbeat interval, echoed so the plugin can calculate staleness
	HeartbeatSeconds int32 `json:"heartbeatSeconds,omitempty"`

	// Neighbors is the list of BGP neighbor session states
	// +kubebuilder:validation:MaxItems=128
	Neighbors []NeighborStatus `json:"neighbors,omitempty"`
	// NeighborCount is the total number of configured neighbors
	NeighborCount int `json:"neighborCount"`

	// NetlinkImport reports the netlink import pipeline state
	NetlinkImport *NetlinkImportStatus `json:"netlinkImport,omitempty"`
	// RIB reports the BGP RIB summary
	RIB *RIBStatus `json:"rib,omitempty"`
	// NetlinkExport reports the netlink export pipeline state
	NetlinkExport *NetlinkExportStatus `json:"netlinkExport,omitempty"`

	// VRFs reports the VRF summary
	// +kubebuilder:validation:MaxItems=64
	VRFs []VRFStatus `json:"vrfs,omitempty"`

	// Truncated indicates the neighbors or vrfs list was capped at its maximum.
	// The nested rib/netlinkImport/netlinkExport objects carry their own flag.
	// +optional
	Truncated bool `json:"truncated,omitempty"`

	// BFDServer reports the node's BFD server, when BFD is in use.
	// +optional
	BFDServer *BFDServerStatus `json:"bfdServer,omitempty"`

	// Healthy is true when all neighbors are Established, no import/export
	// failures exist, and BFD - if any neighbor uses it - is working.
	// Conditions are authoritative; this is a convenience summary.
	//
	// No omitempty: it would drop the field when false, and the printcolumn
	// then renders an unhealthy node as a BLANK cell rather than "false" -
	// indistinguishable from "not reported yet" in `kubectl get bgpnodestatus`,
	// which is the single place an operator looks first.
	Healthy bool `json:"healthy"`
	// Conditions represent the latest available observations of the node's BGP state
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// NeighborStatus reports the state of a single BGP neighbor session.
// +kubebuilder:object:generate=true
type NeighborStatus struct {
	// Address is the neighbor's IP address
	Address string `json:"address"`
	// PeerASN is the neighbor's Autonomous System Number
	PeerASN uint32 `json:"peerASN"`
	// LocalASN is the local AS used for this session
	LocalASN uint32 `json:"localASN"`
	// State is the BGP FSM state: Idle, Connect, Active, OpenSent, OpenConfirm, Established
	State string `json:"state"`
	// SessionUpSince is when the session entered Established state
	SessionUpSince *metav1.Time `json:"sessionUpSince,omitempty"`
	// PrefixesSent is the number of prefixes advertised to this neighbor
	PrefixesSent uint64 `json:"prefixesSent"`
	// PrefixesReceived is the number of prefixes received from this neighbor
	PrefixesReceived uint64 `json:"prefixesReceived"`
	// Description is the configured neighbor description
	Description string `json:"description,omitempty"`
	// LastError is the BGP notification code/subcode for non-Established neighbors
	LastError string `json:"lastError,omitempty"`
	// InheritedBlocks names the configuration blocks this neighbor took from its
	// peer group rather than stating itself, sorted.
	//
	// ListPeer reports the *resolved* configuration, so a value the neighbor set
	// and a value it inherited look identical - which is what made every
	// peer-group defect in this controller hard to see. This answers "why does
	// this peer have this value" without an operator having to diff the CR
	// against a group by hand.
	//
	// Derived from the CR, not from gobgpd's GetRunningConfig provenance: the CR
	// is authoritative about what the operator actually stated, and the daemon's
	// version is a human-readable comment block appended to a 30 KB config
	// document, which is neither cheap to fetch per reconcile nor a stable format
	// to parse.
	// +optional
	InheritedBlocks []string `json:"inheritedBlocks,omitempty"`
	// BFD reports the BFD session, when BFD is enabled for this neighbor.
	// +optional
	BFD *BFDStatus `json:"bfd,omitempty"`
}

// BFDStatus reports the BFD session state for one neighbor.
//
// Deliberately no packet counters. They advance every second, which would make
// statusEqual false on every collection and turn the change-suppression path
// into dead code - and a monotonically increasing counter belongs on the scrape,
// not in a CR. They are bgp_peer_bfd_{transmitted,received}_packets_total.
//
// Note this is a 60s-granularity view of a subsystem that detects failure in
// under a second. It tells you what BFD is configured to do and whether it has
// been failing; it is not the failure detector. For ground truth use
// `gobgp neighbor <addr>`.
// +kubebuilder:object:generate=true
type BFDStatus struct {
	// SessionState is our view: Up, Down, AdminDown, Init or Unspecified.
	SessionState string `json:"sessionState"`
	// RemoteSessionState is the peer's view of the session.
	// +optional
	RemoteSessionState string `json:"remoteSessionState,omitempty"`
	// LocalDiagnosticCode is why our side last changed state.
	//
	// No omitempty: NoDiagnostic is code 0 and a perfectly healthy value, so
	// omitting it when empty would hide the normal case.
	LocalDiagnosticCode string `json:"localDiagnosticCode"`
	// RemoteDiagnosticCode is why the peer's side last changed state.
	RemoteDiagnosticCode string `json:"remoteDiagnosticCode"`
	// FailureTransitions counts up-to-down transitions. Worth keeping even at a
	// 60s heartbeat: a flap that starts and ends inside the window is invisible
	// in sessionState but visible here. Each one is a hard BGP reset.
	FailureTransitions uint64 `json:"failureTransitions"`
}

// BFDServerStatus reports the node's BFD server.
// +kubebuilder:object:generate=true
type BFDServerStatus struct {
	// Listening is whether the BFD socket is bound.
	//
	// The socket binds lazily, on the first BFD peer, so false is the normal
	// state on a node that does not use BFD - which is why this does not feed
	// the top-level Healthy flag unconditionally.
	Listening bool `json:"listening"`
}

// NetlinkImportStatus reports the state of the netlink import pipeline.
// +kubebuilder:object:generate=true
type NetlinkImportStatus struct {
	// Enabled indicates whether netlink import is active
	Enabled bool `json:"enabled"`
	// VRF is the VRF that routes are imported into (empty = global RIB)
	VRF string `json:"vrf,omitempty"`
	// Interfaces reports the state of each watched import interface
	Interfaces []ImportInterfaceStatus `json:"interfaces,omitempty"`
	// ImportedAddresses lists addresses imported from interfaces
	ImportedAddresses []ImportedAddress `json:"importedAddresses,omitempty"`
	// TotalImported is the total count of imported addresses (may exceed len(ImportedAddresses) if truncated)
	TotalImported int `json:"totalImported"`
	// Truncated indicates the importedAddresses list was capped at the maximum
	Truncated bool `json:"truncated,omitempty"`
}

// ImportInterfaceStatus reports the state of a single import interface.
// +kubebuilder:object:generate=true
type ImportInterfaceStatus struct {
	// Name is the interface name (e.g., "kube-lb0")
	Name string `json:"name"`
	// Exists indicates whether the interface exists on this node
	Exists bool `json:"exists"`
	// OperState is the operational state: "up" or "down"
	OperState string `json:"operState"`
}

// ImportedAddress reports a single address imported from an interface.
// +kubebuilder:object:generate=true
type ImportedAddress struct {
	// Address is the imported IP address with prefix length (e.g., "10.100.0.1/32")
	Address string `json:"address"`
	// Interface is the source interface name
	Interface string `json:"interface"`
	// InRIB indicates whether this address exists as a route in the BGP RIB
	InRIB bool `json:"inRIB"`
}

// RIBStatus reports the BGP RIB summary.
// +kubebuilder:object:generate=true
type RIBStatus struct {
	// LocalRoutes lists routes originating from this node
	LocalRoutes []RIBRoute `json:"localRoutes,omitempty"`
	// LocalRouteCount is the total count of local routes (may exceed len(LocalRoutes) if truncated)
	LocalRouteCount int `json:"localRouteCount"`
	// ReceivedRoutes lists routes received from BGP peers
	ReceivedRoutes []RIBRoute `json:"receivedRoutes,omitempty"`
	// ReceivedRouteCount is the total count of received routes (may exceed len(ReceivedRoutes) if truncated)
	ReceivedRouteCount int `json:"receivedRouteCount"`
	// Truncated indicates a route list was capped at the maximum
	Truncated bool `json:"truncated,omitempty"`
}

// RIBRoute reports a single route in the BGP RIB.
// +kubebuilder:object:generate=true
type RIBRoute struct {
	// Prefix is the IP prefix (e.g., "10.100.0.1/32" or "2001:db8::/32")
	Prefix string `json:"prefix"`
	// NextHop is the route's next-hop address
	NextHop string `json:"nextHop"`
	// FromPeer is the peer that advertised this route (only for received routes)
	FromPeer string `json:"fromPeer,omitempty"`
	// AdvertisedTo lists neighbor addresses this route is advertised to (only
	// for local routes), capped at 16 entries. This list is the product of
	// routes and established neighbors, so it, not the route count, is what
	// pushes the object towards etcd's size limit.
	// +kubebuilder:validation:MaxItems=16
	// +optional
	AdvertisedTo []string `json:"advertisedTo,omitempty"`
	// AdvertisedToCount is the true number of neighbors this route is
	// advertised to, which exceeds len(advertisedTo) when that list was capped.
	// +optional
	AdvertisedToCount int `json:"advertisedToCount,omitempty"`
	// Communities lists BGP communities attached to this route
	Communities []string `json:"communities,omitempty"`
}

// NetlinkExportStatus reports the state of the netlink export pipeline.
// +kubebuilder:object:generate=true
type NetlinkExportStatus struct {
	// Enabled indicates whether netlink export is active
	Enabled bool `json:"enabled"`
	// Protocol is the Linux route protocol identifier (e.g., 186 = RTPROT_BGP)
	Protocol int32 `json:"protocol"`
	// Rules lists the configured export rules
	Rules []ExportRuleStatus `json:"rules,omitempty"`
	// ExportedRoutes lists routes exported to the Linux kernel
	ExportedRoutes []ExportedRoute `json:"exportedRoutes,omitempty"`
	// TotalExported is the total count of exported routes (may exceed len(ExportedRoutes) if truncated)
	TotalExported int `json:"totalExported"`
	// Truncated indicates the exportedRoutes list was capped at the maximum
	Truncated bool `json:"truncated,omitempty"`
}

// ExportRuleStatus reports the configuration of a single export rule.
// +kubebuilder:object:generate=true
type ExportRuleStatus struct {
	// Name is the export rule name
	Name string `json:"name"`
	// Metric is the route metric in the Linux routing table
	Metric uint32 `json:"metric"`
	// TableID is the Linux routing table ID
	TableID int32 `json:"tableID"`
}

// ExportedRoute reports a single route exported to the Linux kernel.
// +kubebuilder:object:generate=true
type ExportedRoute struct {
	// Prefix is the exported IP prefix
	Prefix string `json:"prefix"`
	// Table is the Linux routing table name (e.g., "main")
	Table string `json:"table"`
	// Metric is the route metric in the Linux routing table
	Metric uint32 `json:"metric"`
	// Installed indicates whether this route was successfully installed in the kernel
	Installed bool `json:"installed"`
	// Reason explains why a route was not installed (e.g., "nexthop unreachable")
	Reason string `json:"reason,omitempty"`
}

// VRFStatus reports the summary state of a single VRF.
// +kubebuilder:object:generate=true
type VRFStatus struct {
	// Name is the VRF name
	Name string `json:"name"`
	// RD is the route distinguisher (e.g., "65000:100")
	RD string `json:"rd"`
	// ImportedRouteCount is the number of routes in this VRF's RIB
	ImportedRouteCount int `json:"importedRouteCount"`
	// ExportedRouteCount is the number of routes exported to the kernel for this VRF
	ExportedRouteCount int `json:"exportedRouteCount"`
}

// NodeStatusConfig configures per-node BGPNodeStatus reporting.
// +kubebuilder:object:generate=true
type NodeStatusConfig struct {
	// Enabled activates BGPNodeStatus reporting. Default: true.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// HeartbeatSeconds is the interval for unconditional status writes even when state is unchanged.
	// Default: 60. Minimum: 10.
	// +kubebuilder:validation:Minimum=10
	// +optional
	HeartbeatSeconds *int32 `json:"heartbeatSeconds,omitempty"`
}
