package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:object:generate=true
type BGPConfiguration struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              BGPConfigurationSpec   `json:"spec,omitempty"`
	Status            BGPConfigurationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:generate=true
type BGPConfigurationSpec struct {
	Global           GlobalSpec        `json:"global"`
	Neighbors        []Neighbor        `json:"neighbors,omitempty"`
	PeerGroups       []PeerGroup       `json:"peerGroups,omitempty"`
	DynamicNeighbors []DynamicNeighbor `json:"dynamicNeighbors,omitempty"`
	Vrfs             []Vrf             `json:"vrfs,omitempty"`
	// Policies and other configurations will be added in subsequent phases
}

// --- Global ---
// +kubebuilder:object:generate=true
type GlobalSpec struct {
	ASN                   uint32                 `json:"asn"`
	RouterID              string                 `json:"routerID"`
	ListenPort            int32                  `json:"listenPort,omitempty"`
	ListenAddresses       []string               `json:"listenAddresses,omitempty"`
	Families              []uint32               `json:"families,omitempty"`
	UseMultiplePaths      bool                   `json:"useMultiplePaths,omitempty"`
	RouteSelectionOptions *RouteSelectionOptions `json:"routeSelectionOptions,omitempty"`
	DefaultRouteDistance  *DefaultRouteDistance  `json:"defaultRouteDistance,omitempty"`
	Confederation         *Confederation         `json:"confederation,omitempty"`
	GracefulRestart       *GracefulRestart       `json:"gracefulRestart,omitempty"`
	ApplyPolicy           *ApplyPolicy           `json:"applyPolicy,omitempty"`
	BindToDevice          string                 `json:"bindToDevice,omitempty"`
}

// +kubebuilder:object:generate=true
type RouteSelectionOptions struct {
	AlwaysCompareMed        bool `json:"alwaysCompareMed,omitempty"`
	IgnoreAsPathLength      bool `json:"ignoreAsPathLength,omitempty"`
	ExternalCompareRouterId bool `json:"externalCompareRouterId,omitempty"`
	AdvertiseInactiveRoutes bool `json:"advertiseInactiveRoutes,omitempty"`
	EnableAigp              bool `json:"enableAigp,omitempty"`
	IgnoreNextHopIgpMetric  bool `json:"ignoreNextHopIgpMetric,omitempty"`
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

// --- Neighbors ---
// +kubebuilder:object:generate=true
type Neighbor struct {
	Config NeighborConfig `json:"config"`
	// Other neighbor properties like Timers, Transport, etc. can be added here
}

// +kubebuilder:object:generate=true
type NeighborConfig struct {
	AuthPassword      string `json:"authPassword,omitempty"`
	Description       string `json:"description,omitempty"`
	LocalAsn          uint32 `json:"localAsn,omitempty"`
	NeighborAddress   string `json:"neighborAddress"`
	PeerAsn           uint32 `json:"peerAsn"`
	PeerGroup         string `json:"peerGroup,omitempty"`
	PeerType          string `json:"peerType,omitempty"` // INTERNAL or EXTERNAL
	RemovePrivate     string `json:"removePrivate,omitempty"` // REMOVE_NONE, REMOVE_ALL, or REPLACE
	RouteFlapDamping  bool   `json:"routeFlapDamping,omitempty"`
	SendCommunity     uint32 `json:"sendCommunity,omitempty"`
	NeighborInterface string `json:"neighborInterface,omitempty"`
	Vrf               string `json:"vrf,omitempty"`
	AllowOwnAsn       uint32 `json:"allowOwnAsn,omitempty"`
	ReplacePeerAsn    bool   `json:"replacePeerAsn,omitempty"`
	AdminDown         bool   `json:"adminDown,omitempty"`
}

// --- Peer Groups ---
// +kubebuilder:object:generate=true
type PeerGroup struct {
	Config PeerGroupConfig `json:"config"`
	// Other peer group properties can be added here
}

// +kubebuilder:object:generate=true
type PeerGroupConfig struct {
	AuthPassword    string `json:"authPassword,omitempty"`
	Description     string `json:"description,omitempty"`
	LocalAsn        uint32 `json:"localAsn,omitempty"`
	PeerAsn         uint32 `json:"peerAsn,omitempty"`
	PeerGroupName   string `json:"peerGroupName"`
	PeerType        string `json:"peerType,omitempty"`
	RemovePrivate   string `json:"removePrivate,omitempty"`
	RouteFlapDamping bool  `json:"routeFlapDamping,omitempty"`
	SendCommunity   uint32 `json:"sendCommunity,omitempty"`
}

// --- Dynamic Neighbors ---
// +kubebuilder:object:generate=true
type DynamicNeighbor struct {
	Prefix    string `json:"prefix"`
	PeerGroup string `json:"peerGroup"`
}

// --- VRFs ---
// +kubebuilder:object:generate=true
type Vrf struct {
	Name     string   `json:"name"`
	Rd       string   `json:"rd,omitempty"` // Route Distinguisher
	ImportRt []string `json:"importRt,omitempty"`
	ExportRt []string `json:"exportRt,omitempty"`
	Id       uint32   `json:"id,omitempty"`
}

// --- Shared ---
// +kubebuilder:object:generate=true
type GracefulRestart struct {
	Enabled      bool   `json:"enabled,omitempty"`
	RestartTime  uint32 `json:"restartTime,omitempty"`
	HelperOnly   bool   `json:"helperOnly,omitempty"`
	DeferralTime uint32 `json:"deferralTime,omitempty"`
	Mode         string `json:"mode,omitempty"`
}

// +kubebuilder:object:generate=true
type ApplyPolicy struct {
	InPolicy     *PolicyAssignment `json:"inPolicy,omitempty"`
	ExportPolicy *PolicyAssignment `json:"exportPolicy,omitempty"`
	ImportPolicy *PolicyAssignment `json:"importPolicy,omitempty"`
}

// +kubebuilder:object:generate=true
type PolicyAssignment struct {
	Name          string   `json:"name,omitempty"`
	Direction     string   `json:"direction,omitempty"` // IMPORT or EXPORT
	Policies      []string `json:"policies,omitempty"`
	DefaultAction string   `json:"defaultAction,omitempty"` // ACCEPT or REJECT
}

// --- Status ---
// +kubebuilder:object:generate=true
type BGPConfigurationStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:object:generate=true
type BGPConfigurationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BGPConfiguration `json:"items"`
}
