package v1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestValidateGlobalSpec_ValidConfig(t *testing.T) {
	bgpConfig := &BGPConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
		},
		Spec: BGPConfigurationSpec{
			Global: GlobalSpec{
				ASN:        64512,
				RouterID:   "192.168.1.1",
				ListenPort: 179,
			},
		},
	}

	warnings, err := bgpConfig.validateBGPConfiguration()
	assert.NoError(t, err)
	assert.Empty(t, warnings)
}

func TestValidateGlobalSpec_MissingASN(t *testing.T) {
	bgpConfig := &BGPConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
		},
		Spec: BGPConfigurationSpec{
			Global: GlobalSpec{
				RouterID: "192.168.1.1",
			},
		},
	}

	_, err := bgpConfig.validateBGPConfiguration()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "asn")
}

func TestValidateGlobalSpec_InvalidRouterID(t *testing.T) {
	bgpConfig := &BGPConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
		},
		Spec: BGPConfigurationSpec{
			Global: GlobalSpec{
				ASN:      64512,
				RouterID: "not-an-ip",
			},
		},
	}

	_, err := bgpConfig.validateBGPConfiguration()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "routerID")
}

func TestValidateGlobalSpec_IPv6RouterID(t *testing.T) {
	bgpConfig := &BGPConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
		},
		Spec: BGPConfigurationSpec{
			Global: GlobalSpec{
				ASN:      64512,
				RouterID: "::1", // IPv6 not allowed for router ID
			},
		},
	}

	_, err := bgpConfig.validateBGPConfiguration()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "routerID")
}

func TestValidateNeighbor_ValidConfig(t *testing.T) {
	bgpConfig := &BGPConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
		},
		Spec: BGPConfigurationSpec{
			Global: GlobalSpec{
				ASN:      64512,
				RouterID: "192.168.1.1",
			},
			Neighbors: []Neighbor{
				{
					Config: NeighborConfig{
						NeighborAddress: "192.168.1.254",
						PeerAsn:         64513,
					},
				},
			},
		},
	}

	warnings, err := bgpConfig.validateBGPConfiguration()
	assert.NoError(t, err)
	assert.Empty(t, warnings)
}

func TestValidateNeighbor_MissingAddress(t *testing.T) {
	bgpConfig := &BGPConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
		},
		Spec: BGPConfigurationSpec{
			Global: GlobalSpec{
				ASN:      64512,
				RouterID: "192.168.1.1",
			},
			Neighbors: []Neighbor{
				{
					Config: NeighborConfig{
						PeerAsn: 64513,
					},
				},
			},
		},
	}

	_, err := bgpConfig.validateBGPConfiguration()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "neighborAddress")
}

func TestValidateNeighbor_InvalidAddress(t *testing.T) {
	bgpConfig := &BGPConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
		},
		Spec: BGPConfigurationSpec{
			Global: GlobalSpec{
				ASN:      64512,
				RouterID: "192.168.1.1",
			},
			Neighbors: []Neighbor{
				{
					Config: NeighborConfig{
						NeighborAddress: "invalid-ip",
						PeerAsn:         64513,
					},
				},
			},
		},
	}

	_, err := bgpConfig.validateBGPConfiguration()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "neighborAddress")
}

func TestValidateNeighbor_DeprecatedPassword(t *testing.T) {
	bgpConfig := &BGPConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
		},
		Spec: BGPConfigurationSpec{
			Global: GlobalSpec{
				ASN:      64512,
				RouterID: "192.168.1.1",
			},
			Neighbors: []Neighbor{
				{
					Config: NeighborConfig{
						NeighborAddress: "192.168.1.254",
						PeerAsn:         64513,
						AuthPassword:    "secret", // Deprecated
					},
				},
			},
		},
	}

	warnings, err := bgpConfig.validateBGPConfiguration()
	assert.NoError(t, err)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "deprecated")
}

func TestValidateNeighbor_InvalidHoldTime(t *testing.T) {
	bgpConfig := &BGPConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
		},
		Spec: BGPConfigurationSpec{
			Global: GlobalSpec{
				ASN:      64512,
				RouterID: "192.168.1.1",
			},
			Neighbors: []Neighbor{
				{
					Config: NeighborConfig{
						NeighborAddress: "192.168.1.254",
						PeerAsn:         64513,
					},
					Timers: &Timers{
						Config: TimersConfig{
							HoldTime: 2, // Must be 0 or >= 3
						},
					},
				},
			},
		},
	}

	_, err := bgpConfig.validateBGPConfiguration()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "holdTime")
}

func TestValidateVrf_ValidConfig(t *testing.T) {
	bgpConfig := &BGPConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
		},
		Spec: BGPConfigurationSpec{
			Global: GlobalSpec{
				ASN:      64512,
				RouterID: "192.168.1.1",
			},
			Vrfs: []Vrf{
				{
					Name:     "red",
					Rd:       "65000:100",
					ImportRt: []string{"65000:100"},
					ExportRt: []string{"65000:100"},
				},
			},
		},
	}

	warnings, err := bgpConfig.validateBGPConfiguration()
	assert.NoError(t, err)
	assert.Empty(t, warnings)
}

func TestValidateVrf_InvalidRd(t *testing.T) {
	bgpConfig := &BGPConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
		},
		Spec: BGPConfigurationSpec{
			Global: GlobalSpec{
				ASN:      64512,
				RouterID: "192.168.1.1",
			},
			Vrfs: []Vrf{
				{
					Name: "red",
					Rd:   "invalid-rd",
				},
			},
		},
	}

	_, err := bgpConfig.validateBGPConfiguration()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rd")
}

func TestValidateDynamicNeighbor_ValidConfig(t *testing.T) {
	bgpConfig := &BGPConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
		},
		Spec: BGPConfigurationSpec{
			Global: GlobalSpec{
				ASN:      64512,
				RouterID: "192.168.1.1",
			},
			PeerGroups: []PeerGroup{
				{
					Config: PeerGroupConfig{
						PeerGroupName: "dynamic-peers",
					},
				},
			},
			DynamicNeighbors: []DynamicNeighbor{
				{
					Prefix:    "192.168.0.0/24",
					PeerGroup: "dynamic-peers",
				},
			},
		},
	}

	warnings, err := bgpConfig.validateBGPConfiguration()
	assert.NoError(t, err)
	assert.Empty(t, warnings)
}

func TestValidateDynamicNeighbor_InvalidPrefix(t *testing.T) {
	bgpConfig := &BGPConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
		},
		Spec: BGPConfigurationSpec{
			Global: GlobalSpec{
				ASN:      64512,
				RouterID: "192.168.1.1",
			},
			PeerGroups: []PeerGroup{
				{
					Config: PeerGroupConfig{
						PeerGroupName: "dynamic-peers",
					},
				},
			},
			DynamicNeighbors: []DynamicNeighbor{
				{
					Prefix:    "not-a-cidr",
					PeerGroup: "dynamic-peers",
				},
			},
		},
	}

	_, err := bgpConfig.validateBGPConfiguration()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "prefix")
}

func TestValidateDynamicNeighbor_MissingPeerGroup(t *testing.T) {
	bgpConfig := &BGPConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
		},
		Spec: BGPConfigurationSpec{
			Global: GlobalSpec{
				ASN:      64512,
				RouterID: "192.168.1.1",
			},
			DynamicNeighbors: []DynamicNeighbor{
				{
					Prefix:    "192.168.0.0/24",
					PeerGroup: "non-existent",
				},
			},
		},
	}

	_, err := bgpConfig.validateBGPConfiguration()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "peerGroup")
}

func TestValidatePolicy_ValidConfig(t *testing.T) {
	bgpConfig := &BGPConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
		},
		Spec: BGPConfigurationSpec{
			Global: GlobalSpec{
				ASN:      64512,
				RouterID: "192.168.1.1",
			},
			PolicyDefinitions: []PolicyDefinition{
				{
					Name: "accept-all",
					Statements: []Statement{
						{
							Name: "stmt1",
							Actions: Actions{
								RouteAction: "accept",
							},
						},
					},
				},
			},
		},
	}

	warnings, err := bgpConfig.validateBGPConfiguration()
	assert.NoError(t, err)
	assert.Empty(t, warnings)
}

func TestValidatePolicy_InvalidAction(t *testing.T) {
	bgpConfig := &BGPConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
		},
		Spec: BGPConfigurationSpec{
			Global: GlobalSpec{
				ASN:      64512,
				RouterID: "192.168.1.1",
			},
			PolicyDefinitions: []PolicyDefinition{
				{
					Name: "test-policy",
					Statements: []Statement{
						{
							Name: "stmt1",
							Actions: Actions{
								RouteAction: "invalid-action",
							},
						},
					},
				},
			},
		},
	}

	_, err := bgpConfig.validateBGPConfiguration()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "routeAction")
}

func TestIsValidFamily(t *testing.T) {
	tests := []struct {
		family   string
		expected bool
	}{
		{"ipv4-unicast", true},
		{"ipv6-unicast", true},
		{"l2vpn-evpn", true},
		{"IPV4-UNICAST", true}, // Case insensitive
		{"invalid", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.family, func(t *testing.T) {
			result := isValidFamily(tt.family)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestIsValidName(t *testing.T) {
	tests := []struct {
		name     string
		expected bool
	}{
		{"valid-name", true},
		{"valid_name", true},
		{"ValidName123", true},
		{"invalid name", false},  // Space
		{"invalid.name", false},  // Dot
		{"invalid@name", false},  // Special char
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isValidName(tt.name)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestIsValidRouteDistinguisher(t *testing.T) {
	tests := []struct {
		rd       string
		expected bool
	}{
		{"65000:100", true},
		{"192.168.1.1:100", true},
		{"invalid", false},
		{"65000", false},
		{"65000:100:200", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.rd, func(t *testing.T) {
			result := isValidRouteDistinguisher(tt.rd)
			assert.Equal(t, tt.expected, result)
		})
	}
}
