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
	// +kubebuilder:validation:Required
	Global    GlobalSpec `json:"global"`
	Neighbors []Neighbor `json:"neighbors,omitempty"`
}

// +kubebuilder:object:generate=true
type GlobalSpec struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=4294967295
	ASN uint32 `json:"asn"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Format=ipv4
	RouterID string `json:"routerID"`
}

// +kubebuilder:object:generate=true
type Neighbor struct {
	// +kubebuilder:validation:Required
	Config NeighborConfig `json:"config"`
}

// +kubebuilder:object:generate=true
type NeighborConfig struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Format=ip
	NeighborAddress string `json:"neighborAddress"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=4294967295
	PeerAs uint32 `json:"peerAs"`
}

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
