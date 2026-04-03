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
	Neighbors []NeighborStatus `json:"neighbors,omitempty"`
	// NeighborCount is the total number of configured neighbors
	NeighborCount int `json:"neighborCount,omitempty"`

	// NetlinkImport reports the netlink import pipeline state
	NetlinkImport *NetlinkImportStatus `json:"netlinkImport,omitempty"`
	// RIB reports the BGP RIB summary
	RIB *RIBStatus `json:"rib,omitempty"`
	// NetlinkExport reports the netlink export pipeline state
	NetlinkExport *NetlinkExportStatus `json:"netlinkExport,omitempty"`

	// VRFs reports the VRF summary
	VRFs []VRFStatus `json:"vrfs,omitempty"`

	// Healthy is true when all neighbors are Established and no import/export failures exist.
	// Conditions are authoritative; this is a convenience summary.
	Healthy bool `json:"healthy,omitempty"`
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
	// DampeningInterval is the dampening interval in milliseconds
	DampeningInterval uint32 `json:"dampeningInterval"`
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
