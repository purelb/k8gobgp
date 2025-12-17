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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	gobgpapi "github.com/osrg/gobgp/v4/api"
	bgpv1 "github.com/purelb/k8gobgp/api/v1"
)

func TestCrdToAPIGlobal(t *testing.T) {
	tests := []struct {
		name     string
		input    *bgpv1.GlobalSpec
		expected *gobgpapi.Global
	}{
		{
			name: "basic global config",
			input: &bgpv1.GlobalSpec{
				ASN:      64512,
				RouterID: "192.168.1.1",
			},
			expected: &gobgpapi.Global{
				Asn:      64512,
				RouterId: "192.168.1.1",
			},
		},
		{
			name: "global config with listen port",
			input: &bgpv1.GlobalSpec{
				ASN:             64512,
				RouterID:        "10.0.0.1",
				ListenPort:      179,
				ListenAddresses: []string{"0.0.0.0", "::"},
			},
			expected: &gobgpapi.Global{
				Asn:             64512,
				RouterId:        "10.0.0.1",
				ListenPort:      179,
				ListenAddresses: []string{"0.0.0.0", "::"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := crdToAPIGlobal(tt.input)
			assert.Equal(t, tt.expected.Asn, result.Asn)
			assert.Equal(t, tt.expected.RouterId, result.RouterId)
			assert.Equal(t, tt.expected.ListenPort, result.ListenPort)
			assert.Equal(t, tt.expected.ListenAddresses, result.ListenAddresses)
		})
	}
}

func TestCrdToAPIFamily(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected *gobgpapi.Family
	}{
		{
			name:  "ipv4-unicast",
			input: "ipv4-unicast",
			expected: &gobgpapi.Family{
				Afi:  gobgpapi.Family_AFI_IP,
				Safi: gobgpapi.Family_SAFI_UNICAST,
			},
		},
		{
			name:  "ipv6-unicast",
			input: "ipv6-unicast",
			expected: &gobgpapi.Family{
				Afi:  gobgpapi.Family_AFI_IP6,
				Safi: gobgpapi.Family_SAFI_UNICAST,
			},
		},
		{
			name:  "l2vpn-evpn",
			input: "l2vpn-evpn",
			expected: &gobgpapi.Family{
				Afi:  gobgpapi.Family_AFI_L2VPN,
				Safi: gobgpapi.Family_SAFI_EVPN,
			},
		},
		{
			name:  "unknown defaults to unspecified",
			input: "unknown",
			expected: &gobgpapi.Family{
				Afi:  gobgpapi.Family_AFI_UNSPECIFIED,
				Safi: gobgpapi.Family_SAFI_UNSPECIFIED,
			},
		},
		{
			name:  "case insensitive",
			input: "IPV4-UNICAST",
			expected: &gobgpapi.Family{
				Afi:  gobgpapi.Family_AFI_IP,
				Safi: gobgpapi.Family_SAFI_UNICAST,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := crdToAPIFamily(tt.input)
			assert.Equal(t, tt.expected.Afi, result.Afi)
			assert.Equal(t, tt.expected.Safi, result.Safi)
		})
	}
}

func TestCrdToAPIRouteAction(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected gobgpapi.RouteAction
	}{
		{
			name:     "accept",
			input:    "accept",
			expected: gobgpapi.RouteAction_ROUTE_ACTION_ACCEPT,
		},
		{
			name:     "reject",
			input:    "reject",
			expected: gobgpapi.RouteAction_ROUTE_ACTION_REJECT,
		},
		{
			name:     "unknown defaults to unspecified",
			input:    "unknown",
			expected: gobgpapi.RouteAction_ROUTE_ACTION_UNSPECIFIED,
		},
		{
			name:     "case insensitive",
			input:    "ACCEPT",
			expected: gobgpapi.RouteAction_ROUTE_ACTION_ACCEPT,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := crdToAPIRouteAction(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestCrdToAPIDefinedType(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected gobgpapi.DefinedType
	}{
		{
			name:     "prefix",
			input:    "prefix",
			expected: gobgpapi.DefinedType_DEFINED_TYPE_PREFIX,
		},
		{
			name:     "neighbor",
			input:    "neighbor",
			expected: gobgpapi.DefinedType_DEFINED_TYPE_NEIGHBOR,
		},
		{
			name:     "as-path",
			input:    "as-path",
			expected: gobgpapi.DefinedType_DEFINED_TYPE_AS_PATH,
		},
		{
			name:     "community",
			input:    "community",
			expected: gobgpapi.DefinedType_DEFINED_TYPE_COMMUNITY,
		},
		{
			name:     "unknown defaults to prefix",
			input:    "unknown",
			expected: gobgpapi.DefinedType_DEFINED_TYPE_PREFIX,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := crdToAPIDefinedType(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestParseRouteDistinguisher(t *testing.T) {
	tests := []struct {
		name             string
		input            string
		expectNil        bool
		expectedAdmin    uint32
		expectedAssigned uint32
	}{
		{
			name:             "valid RD",
			input:            "65000:100",
			expectNil:        false,
			expectedAdmin:    65000,
			expectedAssigned: 100,
		},
		{
			name:      "empty string",
			input:     "",
			expectNil: true,
		},
		{
			name:      "invalid format no colon",
			input:     "65000100",
			expectNil: true,
		},
		{
			name:      "invalid format too many colons",
			input:     "65000:100:200",
			expectNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseRouteDistinguisher(tt.input)
			if tt.expectNil {
				assert.Nil(t, result)
			} else {
				require.NotNil(t, result)
				require.NotNil(t, result.GetTwoOctetAsn())
				assert.Equal(t, tt.expectedAdmin, result.GetTwoOctetAsn().Admin)
				assert.Equal(t, tt.expectedAssigned, result.GetTwoOctetAsn().Assigned)
			}
		})
	}
}

func TestParseRouteTarget(t *testing.T) {
	tests := []struct {
		name               string
		input              string
		expectNil          bool
		expectedAsn        uint32
		expectedLocalAdmin uint32
	}{
		{
			name:               "valid RT",
			input:              "65000:100",
			expectNil:          false,
			expectedAsn:        65000,
			expectedLocalAdmin: 100,
		},
		{
			name:      "empty string",
			input:     "",
			expectNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseRouteTarget(tt.input)
			if tt.expectNil {
				assert.Nil(t, result)
			} else {
				require.NotNil(t, result)
				require.NotNil(t, result.GetTwoOctetAsSpecific())
				assert.Equal(t, tt.expectedAsn, result.GetTwoOctetAsSpecific().Asn)
				assert.Equal(t, tt.expectedLocalAdmin, result.GetTwoOctetAsSpecific().LocalAdmin)
			}
		})
	}
}

func TestResolveAuthPassword(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = bgpv1.AddToScheme(scheme)

	tests := []struct {
		name           string
		namespace      string
		inlinePassword string
		secretRef      *corev1.SecretKeySelector
		secret         *corev1.Secret
		expected       string
	}{
		{
			name:           "inline password only",
			namespace:      "default",
			inlinePassword: "inline-pass",
			secretRef:      nil,
			expected:       "inline-pass",
		},
		{
			name:           "secret ref takes precedence",
			namespace:      "default",
			inlinePassword: "inline-pass",
			secretRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "bgp-secret"},
				Key:                  "password",
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "bgp-secret",
					Namespace: "default",
				},
				Data: map[string][]byte{
					"password": []byte("secret-pass"),
				},
			},
			expected: "secret-pass",
		},
		{
			name:           "missing secret returns empty",
			namespace:      "default",
			inlinePassword: "",
			secretRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "missing-secret"},
				Key:                  "password",
			},
			expected: "",
		},
		{
			name:           "missing key returns empty",
			namespace:      "default",
			inlinePassword: "",
			secretRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "bgp-secret"},
				Key:                  "wrong-key",
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "bgp-secret",
					Namespace: "default",
				},
				Data: map[string][]byte{
					"password": []byte("secret-pass"),
				},
			},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objs []runtime.Object
			if tt.secret != nil {
				objs = append(objs, tt.secret)
			}
			client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()

			r := &BGPConfigurationReconciler{
				Client: client,
				Log:    zap.New(zap.UseDevMode(true)),
				Scheme: scheme,
			}

			result := r.resolveAuthPassword(context.Background(), tt.namespace, tt.inlinePassword, tt.secretRef, r.Log)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestCrdToAPITimers(t *testing.T) {
	tests := []struct {
		name  string
		input *bgpv1.Timers
		isNil bool
	}{
		{
			name:  "nil input",
			input: nil,
			isNil: true,
		},
		{
			name: "valid timers",
			input: &bgpv1.Timers{
				Config: bgpv1.TimersConfig{
					HoldTime:          90,
					KeepaliveInterval: 30,
					ConnectRetry:      120,
				},
			},
			isNil: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := crdToAPITimers(tt.input)
			if tt.isNil {
				assert.Nil(t, result)
			} else {
				require.NotNil(t, result)
				assert.Equal(t, tt.input.Config.HoldTime, result.Config.HoldTime)
				assert.Equal(t, tt.input.Config.KeepaliveInterval, result.Config.KeepaliveInterval)
				assert.Equal(t, tt.input.Config.ConnectRetry, result.Config.ConnectRetry)
			}
		})
	}
}

func TestCrdToAPITransport(t *testing.T) {
	tests := []struct {
		name  string
		input *bgpv1.Transport
		isNil bool
	}{
		{
			name:  "nil input",
			input: nil,
			isNil: true,
		},
		{
			name: "valid transport",
			input: &bgpv1.Transport{
				LocalAddress:  "192.168.1.1",
				PassiveMode:   true,
				BindInterface: "eth0",
			},
			isNil: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := crdToAPITransport(tt.input)
			if tt.isNil {
				assert.Nil(t, result)
			} else {
				require.NotNil(t, result)
				assert.Equal(t, tt.input.LocalAddress, result.LocalAddress)
				assert.Equal(t, tt.input.PassiveMode, result.PassiveMode)
				assert.Equal(t, tt.input.BindInterface, result.BindInterface)
			}
		})
	}
}

func TestCrdToAPIGracefulRestart(t *testing.T) {
	tests := []struct {
		name  string
		input *bgpv1.GracefulRestart
		isNil bool
	}{
		{
			name:  "nil input",
			input: nil,
			isNil: true,
		},
		{
			name: "valid graceful restart",
			input: &bgpv1.GracefulRestart{
				Enabled:     true,
				RestartTime: 120,
				HelperOnly:  false,
			},
			isNil: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := crdToAPIGracefulRestart(tt.input)
			if tt.isNil {
				assert.Nil(t, result)
			} else {
				require.NotNil(t, result)
				assert.Equal(t, tt.input.Enabled, result.Enabled)
				assert.Equal(t, tt.input.RestartTime, result.RestartTime)
				assert.Equal(t, tt.input.HelperOnly, result.HelperOnly)
			}
		})
	}
}

func TestUpdateStatusCondition(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = bgpv1.AddToScheme(scheme)

	bgpConfig := &bgpv1.BGPConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-config",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: bgpv1.BGPConfigurationSpec{
			Global: bgpv1.GlobalSpec{
				ASN:      64512,
				RouterID: "192.168.1.1",
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(bgpConfig).Build()
	r := &BGPConfigurationReconciler{
		Client: client,
		Log:    zap.New(zap.UseDevMode(true)),
		Scheme: scheme,
	}

	// Test adding a new condition
	r.updateStatusCondition(context.Background(), bgpConfig, "Ready", metav1.ConditionTrue, "TestReason", "Test message")

	require.Len(t, bgpConfig.Status.Conditions, 1)
	assert.Equal(t, "Ready", bgpConfig.Status.Conditions[0].Type)
	assert.Equal(t, metav1.ConditionTrue, bgpConfig.Status.Conditions[0].Status)
	assert.Equal(t, "TestReason", bgpConfig.Status.Conditions[0].Reason)
	assert.Equal(t, "Test message", bgpConfig.Status.Conditions[0].Message)

	// Test updating existing condition
	r.updateStatusCondition(context.Background(), bgpConfig, "Ready", metav1.ConditionFalse, "NewReason", "New message")

	require.Len(t, bgpConfig.Status.Conditions, 1)
	assert.Equal(t, metav1.ConditionFalse, bgpConfig.Status.Conditions[0].Status)
	assert.Equal(t, "NewReason", bgpConfig.Status.Conditions[0].Reason)
}

func TestCrdToAPIVrf(t *testing.T) {
	vrf := &bgpv1.Vrf{
		Name:     "test-vrf",
		Rd:       "65000:100",
		ImportRt: []string{"65000:100", "65000:200"},
		ExportRt: []string{"65000:100"},
	}

	result := crdToAPIVrf(vrf)

	assert.Equal(t, "test-vrf", result.Name)
	require.NotNil(t, result.Rd)
	assert.Len(t, result.ImportRt, 2)
	assert.Len(t, result.ExportRt, 1)
}

func TestFinalizerName(t *testing.T) {
	assert.Equal(t, "bgp.purelb.io/finalizer", finalizerName)
}

func TestDefaultGoBGPEndpoint(t *testing.T) {
	assert.Equal(t, "localhost:50051", defaultGoBGPEndpoint)
}

func TestEnvGoBGPEndpoint(t *testing.T) {
	assert.Equal(t, "GOBGP_ENDPOINT", envGoBGPEndpoint)
}

// TestBGPConfigurationReconcilerFields verifies the reconciler struct has required fields
func TestBGPConfigurationReconcilerFields(t *testing.T) {
	scheme := runtime.NewScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	r := &BGPConfigurationReconciler{
		Client:        client,
		Log:           zap.New(zap.UseDevMode(true)),
		Scheme:        scheme,
		GoBGPEndpoint: "unix:///var/run/gobgp/gobgp.sock",
	}

	assert.NotNil(t, r.Client)
	assert.NotNil(t, r.Log)
	assert.NotNil(t, r.Scheme)
	assert.Equal(t, "unix:///var/run/gobgp/gobgp.sock", r.GoBGPEndpoint)
}

// Test helper function for neighbor password resolution
func TestCrdToAPINeighborWithPassword(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = bgpv1.AddToScheme(scheme)

	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &BGPConfigurationReconciler{
		Client: client,
		Log:    zap.New(zap.UseDevMode(true)),
		Scheme: scheme,
	}

	neighbor := &bgpv1.Neighbor{
		Config: bgpv1.NeighborConfig{
			NeighborAddress: "192.168.1.254",
			PeerAsn:         64513,
			Description:     "Test peer",
		},
		AfiSafis: []bgpv1.AfiSafi{
			{Family: "ipv4-unicast", Enabled: true},
		},
	}

	result := r.crdToAPINeighborWithPassword(neighbor, "test-password")

	assert.Equal(t, "192.168.1.254", result.Conf.NeighborAddress)
	assert.Equal(t, uint32(64513), result.Conf.PeerAsn)
	assert.Equal(t, "Test peer", result.Conf.Description)
	assert.Equal(t, "test-password", result.Conf.AuthPassword)
	assert.Len(t, result.AfiSafis, 1)
}

// Test helper function for peer group password resolution
func TestCrdToAPIPeerGroupWithPassword(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = bgpv1.AddToScheme(scheme)

	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &BGPConfigurationReconciler{
		Client: client,
		Log:    zap.New(zap.UseDevMode(true)),
		Scheme: scheme,
	}

	peerGroup := &bgpv1.PeerGroup{
		Config: bgpv1.PeerGroupConfig{
			PeerGroupName: "upstream-peers",
			PeerAsn:       64513,
			Description:   "Upstream peer group",
		},
		AfiSafis: []bgpv1.AfiSafi{
			{Family: "ipv4-unicast", Enabled: true},
		},
	}

	result := r.crdToAPIPeerGroupWithPassword(peerGroup, "group-password")

	assert.Equal(t, "upstream-peers", result.Conf.PeerGroupName)
	assert.Equal(t, uint32(64513), result.Conf.PeerAsn)
	assert.Equal(t, "Upstream peer group", result.Conf.Description)
	assert.Equal(t, "group-password", result.Conf.AuthPassword)
	assert.Len(t, result.AfiSafis, 1)
}
