package v1

import (
	corev1 "k8s.io/api/core/v1"
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
	NetlinkImport     *NetlinkImport     `json:"netlinkImport,omitempty"`
	NetlinkExport     *NetlinkExport     `json:"netlinkExport,omitempty"`
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
	// CommunityList filters routes by standard BGP communities (format: "AS:VALUE")
	CommunityList []string `json:"communityList,omitempty"`
	// LargeCommunityList filters routes by large BGP communities (format: "ASN:LocalData1:LocalData2")
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
	// AuthPassword is the BGP authentication password (DEPRECATED: use AuthPasswordSecretRef instead)
	// +optional
	AuthPassword string `json:"authPassword,omitempty"`
	// AuthPasswordSecretRef references a Secret containing the BGP authentication password
	// The Secret must contain a key matching the neighbor address or a default key "password"
	// +optional
	AuthPasswordSecretRef *corev1.SecretKeySelector `json:"authPasswordSecretRef,omitempty"`
	Description           string                    `json:"description,omitempty"`
	LocalAsn              uint32                    `json:"localAsn,omitempty"`
	NeighborAddress       string                    `json:"neighborAddress"`
	PeerAsn               uint32                    `json:"peerAsn"`
	PeerGroup             string                    `json:"peerGroup,omitempty"`
	AdminDown             bool                      `json:"adminDown,omitempty"`
	NeighborInterface     string                    `json:"neighborInterface,omitempty"`
	Vrf                   string                    `json:"vrf,omitempty"`
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
	ConnectRetry                 uint64 `json:"connectRetry,omitempty"`
	HoldTime                     uint64 `json:"holdTime,omitempty"`
	KeepaliveInterval            uint64 `json:"keepaliveInterval,omitempty"`
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
	// CommunityList filters routes by standard BGP communities (format: "AS:VALUE")
	CommunityList []string `json:"communityList,omitempty"`
	// LargeCommunityList filters routes by large BGP communities (format: "ASN:LocalData1:LocalData2")
	LargeCommunityList []string `json:"largeCommunityList,omitempty"`
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
	Name  string `json:"name"`
	Match string `json:"match,omitempty"` // "any", "all", "invert"
}

// +kubebuilder:object:generate=true
type Actions struct {
	RouteAction string           `json:"routeAction"` // "accept" or "reject"
	Community   *CommunityAction `json:"community,omitempty"`
	Med         *MedAction       `json:"med,omitempty"`
	AsPrepend   *AsPrependAction `json:"asPrepend,omitempty"`
	LocalPref   uint32           `json:"localPref,omitempty"`
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
	// ObservedGeneration is the most recent generation observed by the controller
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions represent the latest available observations of the BGP configuration state
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// LastReconcileTime is the timestamp of the last reconciliation
	LastReconcileTime *metav1.Time `json:"lastReconcileTime,omitempty"`
	// NeighborCount is the number of configured BGP neighbors
	NeighborCount int `json:"neighborCount,omitempty"`
	// EstablishedNeighbors is the count of neighbors in Established state
	EstablishedNeighbors int `json:"establishedNeighbors,omitempty"`
	// Message provides additional information about the current state
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:object:generate=true
type BGPConfigurationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BGPConfiguration `json:"items"`
}
