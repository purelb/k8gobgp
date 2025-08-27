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
	Global            GlobalSpec         `json:"global"`
	Neighbors         []Neighbor         `json:"neighbors,omitempty"`
	PeerGroups        []PeerGroup        `json:"peerGroups,omitempty"`
	DynamicNeighbors  []DynamicNeighbor  `json:"dynamicNeighbors,omitempty"`
	Vrfs              []Vrf              `json:"vrfs,omitempty"`
	PolicyDefinitions []PolicyDefinition `json:"policyDefinitions,omitempty"`
	DefinedSets       []DefinedSet       `json:"definedSets,omitempty"`
}

// --- Core Config Sections ---

// +kubebuilder:object:generate=true
type GlobalSpec struct {
	ASN                   uint32                 `json:"asn"`
	RouterID              string                 `json:"routerID"`
	ListenPort            int32                  `json:"listenPort,omitempty"`
	ListenAddresses       []string               `json:"listenAddresses,omitempty"`
	Families              []string               `json:"families,omitempty"`
	UseMultiplePaths      bool                   `json:"useMultiplePaths,omitempty"`
	RouteSelectionOptions *RouteSelectionOptions `json:"routeSelectionOptions,omitempty"`
	DefaultRouteDistance  *DefaultRouteDistance  `json:"defaultRouteDistance,omitempty"`
	Confederation         *Confederation         `json:"confederation,omitempty"`
	GracefulRestart       *GracefulRestart       `json:"gracefulRestart,omitempty"`
	ApplyPolicy           *ApplyPolicy           `json:"applyPolicy,omitempty"`
	BindToDevice          string                 `json:"bindToDevice,omitempty"`
}

// +kubebuilder:object:generate=true
type Neighbor struct {
	Config          NeighborConfig   `json:"config"`
	AfiSafis        []AfiSafi        `json:"afiSafis,omitempty"`
	ApplyPolicy     *ApplyPolicy     `json:"applyPolicy,omitempty"`
	Timers          *Timers          `json:"timers,omitempty"`
	Transport       *Transport       `json:"transport,omitempty"`
	GracefulRestart *GracefulRestart `json:"gracefulRestart,omitempty"`
	RouteReflector  *RouteReflector  `json:"routeReflector,omitempty"`
	EbgpMultihop    *EbgpMultihop    `json:"ebgpMultihop,omitempty"`
}

// +kubebuilder:object:generate=true
type PeerGroup struct {
	Config      PeerGroupConfig `json:"config"`
	AfiSafis    []AfiSafi       `json:"afiSafis,omitempty"`
	ApplyPolicy *ApplyPolicy    `json:"applyPolicy,omitempty"`
	Timers      *Timers         `json:"timers,omitempty"`
	Transport   *Transport      `json:"transport,omitempty"`
}

// --- Detailed Sub-structs ---

// +kubebuilder:object:generate=true
type NeighborConfig struct {
	AuthPassword      string `json:"authPassword,omitempty"`
	Description       string `json:"description,omitempty"`
	LocalAsn          uint32 `json:"localAsn,omitempty"`
	NeighborAddress   string `json:"neighborAddress"`
	PeerAsn           uint32 `json:"peerAsn"`
	PeerGroup         string `json:"peerGroup,omitempty"`
	AdminDown         bool   `json:"adminDown,omitempty"`
	NeighborInterface string `json:"neighborInterface,omitempty"`
	Vrf               string `json:"vrf,omitempty"`
}

// +kubebuilder:object:generate=true
type PeerGroupConfig struct {
	PeerGroupName string `json:"peerGroupName"`
	Description   string `json:"description,omitempty"`
	PeerAsn       uint32 `json:"peerAsn,omitempty"`
	LocalAsn      uint32 `json:"localAsn,omitempty"`
	AuthPassword  string `json:"authPassword,omitempty"`
}

// +kubebuilder:object:generate=true
type AfiSafi struct {
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
type Timers struct {
	Config TimersConfig `json:"config"`
}

// +kubebuilder:object:generate=true
type TimersConfig struct {
	ConnectRetry               uint64 `json:"connectRetry,omitempty"`
	HoldTime                   uint64 `json:"holdTime,omitempty"`
	KeepaliveInterval          uint64 `json:"keepaliveInterval,omitempty"`
	MinimumAdvertisementInterval uint64 `json:"minimumAdvertisementInterval,omitempty"`
}

// +kubebuilder:object:generate=true
type Transport struct {
	LocalAddress  string `json:"localAddress,omitempty"`
	PassiveMode   bool   `json:"passiveMode,omitempty"`
	BindInterface string `json:"bindInterface,omitempty"`
}

// +kubebuilder:object:generate=true
type GracefulRestart struct {
	Enabled     bool   `json:"enabled,omitempty"`
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
	Name     string   `json:"name"`
	Rd       string   `json:"rd,omitempty"`
	ImportRt []string `json:"importRt,omitempty"`
	ExportRt []string `json:"exportRt,omitempty"`
}

// +kubebuilder:object:generate=true
type RouteSelectionOptions struct {
	AlwaysCompareMed        bool `json:"alwaysCompareMed,omitempty"`
	AdvertiseInactiveRoutes bool `json:"advertiseInactiveRoutes,omitempty"`
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
	InPolicy     *PolicyAssignment `json:"inPolicy,omitempty"`
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
	Name       string      `json:"name"`
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
}

// +kubebuilder:object:generate=true
type MatchSet struct {
	Name    string `json:"name"`
	Match   string `json:"match,omitempty"` // "any", "all", "invert"
}

// +kubebuilder:object:generate=true
type Actions struct {
	RouteAction   string             `json:"routeAction"` // "accept" or "reject"
	Community     *CommunityAction   `json:"community,omitempty"`
	Med           *MedAction         `json:"med,omitempty"`
	AsPrepend     *AsPrependAction   `json:"asPrepend,omitempty"`
	LocalPref     uint32             `json:"localPref,omitempty"`
}

// +kubebuilder:object:generate=true
type CommunityAction struct {
	Type        string   `json:"type"` // "add", "remove", "replace"
	Communities []string `json:"communities"`
}

// +kubebuilder:object:generate=true
type MedAction struct {
	Type  string `json:"type"` // "mod" or "replace"
	Value int64  `json:"value"`
}

// +kubebuilder:object:generate=true
type AsPrependAction struct {
	Asn    uint32 `json:"asn"`
	Repeat uint32 `json:"repeat,omitempty"`
}

// +kubebuilder:object:generate=true
type DefinedSet struct {
	Type     string   `json:"type"` // "prefix", "neighbor", "as-path", "community"
	Name     string   `json:"name"`
	List     []string `json:"list,omitempty"`
	Prefixes []Prefix `json:"prefixes,omitempty"`
}

// +kubebuilder:object:generate=true
type Prefix struct {
	IpPrefix      string `json:"ipPrefix"`
	MaskLengthMin uint32 `json:"maskLengthMin,omitempty"`
	MaskLengthMax uint32 `json:"maskLengthMax,omitempty"`
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
