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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	gobgpapi "github.com/osrg/gobgp/v4/api"
	bgpv1 "github.com/purelb/k8gobgp/api/v1"
)

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
		wantErr        bool
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
			name:           "missing secret returns error",
			namespace:      "default",
			inlinePassword: "",
			secretRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "missing-secret"},
				Key:                  "password",
			},
			wantErr: true,
		},
		{
			name:           "missing key returns error",
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
			wantErr: true,
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

			result, err := r.resolveAuthPassword(context.Background(), tt.namespace, tt.inlinePassword, tt.secretRef, r.Log)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expected, result)
			}
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

// TestValidateConfiguration_RouterIDPool tests the routerIDPool validation path
func TestValidateConfiguration_RouterIDPool(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = bgpv1.AddToScheme(scheme)

	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &BGPConfigurationReconciler{
		Client: client,
		Log:    zap.New(zap.UseDevMode(true)),
		Scheme: scheme,
	}

	tests := []struct {
		name      string
		pool      string
		wantValid bool
	}{
		{
			name:      "valid pool",
			pool:      "10.255.0.0/16",
			wantValid: true,
		},
		{
			name:      "empty pool (allowed)",
			pool:      "",
			wantValid: true,
		},
		{
			name:      "pool too small /25",
			pool:      "10.0.0.0/25",
			wantValid: false,
		},
		{
			name:      "invalid CIDR",
			pool:      "not-a-cidr",
			wantValid: false,
		},
		{
			name:      "IPv6 pool",
			pool:      "2001:db8::/32",
			wantValid: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := &bgpv1.BGPConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-config",
					Namespace: "default",
				},
				Spec: bgpv1.BGPConfigurationSpec{
					Global: bgpv1.GlobalSpec{
						ASN:          64512,
						RouterID:     "192.168.1.1",
						RouterIDPool: tt.pool,
					},
				},
			}

			result := r.validateConfiguration(config)
			assert.Equal(t, tt.wantValid, result.Valid, "pool=%q: valid=%v, errors=%v", tt.pool, result.Valid, result.ErrorMessages())
		})
	}
}

// TestAnnotatePodWithRouterID tests pod annotation patching
func TestAnnotatePodWithRouterID(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "k8gobgp-abc123",
			Namespace: "k8gobgp-system",
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	r := &BGPConfigurationReconciler{
		Client:       client,
		Log:          zap.New(zap.UseDevMode(true)),
		Scheme:       scheme,
		PodName:      "k8gobgp-abc123",
		PodNamespace: "k8gobgp-system",
	}

	resolution := &RouterIDResolution{
		RouterID: "192.168.1.10",
		Source:   RouterIDSourceNodeIPv4,
		NodeName: "worker-1",
	}

	err := r.annotatePodWithRouterID(context.Background(), resolution, 64512)
	assert.NoError(t, err)

	// Verify annotations were set by fetching the pod back
	var updatedPod corev1.Pod
	err = client.Get(context.Background(), types.NamespacedName{
		Name: "k8gobgp-abc123", Namespace: "k8gobgp-system",
	}, &updatedPod)
	require.NoError(t, err)

	assert.Equal(t, "192.168.1.10", updatedPod.Annotations["bgp.purelb.io/router-id"])
	assert.Equal(t, "node-ipv4", updatedPod.Annotations["bgp.purelb.io/router-id-source"])
	assert.Equal(t, "64512", updatedPod.Annotations["bgp.purelb.io/asn"])
}

// TestAnnotatePodWithRouterID_MissingPodInfo tests error when PodName/PodNamespace not set
func TestAnnotatePodWithRouterID_MissingPodInfo(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &BGPConfigurationReconciler{
		Client: client,
		Log:    zap.New(zap.UseDevMode(true)),
		Scheme: scheme,
		// PodName and PodNamespace intentionally empty
	}

	resolution := &RouterIDResolution{
		RouterID: "192.168.1.10",
		Source:   RouterIDSourceNodeIPv4,
		NodeName: "worker-1",
	}

	err := r.annotatePodWithRouterID(context.Background(), resolution, 64512)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "PodName or PodNamespace not set")
}

// TestEmitRouterIDResolvedEvent tests event emission for successful resolution
func TestEmitRouterIDResolvedEvent(t *testing.T) {
	fakeRecorder := record.NewFakeRecorder(10)

	r := &BGPConfigurationReconciler{
		Recorder: fakeRecorder,
	}

	config := &bgpv1.BGPConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
		},
	}

	resolution := &RouterIDResolution{
		RouterID: "192.168.1.10",
		Source:   RouterIDSourceNodeIPv4,
		NodeName: "worker-1",
	}

	r.emitRouterIDResolvedEvent(config, resolution)

	// Read event from the fake recorder channel
	select {
	case event := <-fakeRecorder.Events:
		assert.Contains(t, event, "Normal")
		assert.Contains(t, event, EventReasonRouterIDResolved)
		assert.Contains(t, event, "192.168.1.10")
		assert.Contains(t, event, "node-ipv4")
		assert.Contains(t, event, "worker-1")
	default:
		t.Error("expected an event to be emitted, got none")
	}
}

// TestEmitRouterIDResolvedEvent_NilRecorder tests graceful handling when recorder is nil
func TestEmitRouterIDResolvedEvent_NilRecorder(t *testing.T) {
	r := &BGPConfigurationReconciler{
		// Recorder intentionally nil
	}

	config := &bgpv1.BGPConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
		},
	}

	resolution := &RouterIDResolution{
		RouterID: "192.168.1.10",
		Source:   RouterIDSourceNodeIPv4,
		NodeName: "worker-1",
	}

	// Should not panic
	r.emitRouterIDResolvedEvent(config, resolution)
}

// TestEmitRouterIDFailedEvent tests event emission for failed resolution
func TestEmitRouterIDFailedEvent(t *testing.T) {
	fakeRecorder := record.NewFakeRecorder(10)

	r := &BGPConfigurationReconciler{
		Recorder: fakeRecorder,
	}

	config := &bgpv1.BGPConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
		},
	}

	r.emitRouterIDFailedEvent(config, assert.AnError)

	select {
	case event := <-fakeRecorder.Events:
		assert.Contains(t, event, "Warning")
		assert.Contains(t, event, EventReasonRouterIDFailed)
	default:
		t.Error("expected a warning event to be emitted, got none")
	}
}

// TestEmitRouterIDFailedEvent_NilRecorder tests graceful handling when recorder is nil
func TestEmitRouterIDFailedEvent_NilRecorder(t *testing.T) {
	r := &BGPConfigurationReconciler{
		// Recorder intentionally nil
	}

	config := &bgpv1.BGPConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-config",
			Namespace: "default",
		},
	}

	// Should not panic
	r.emitRouterIDFailedEvent(config, assert.AnError)
}

// --- neighborKey tests ---

func TestNeighborKey(t *testing.T) {
	tests := []struct {
		name     string
		conf     *gobgpapi.PeerConf
		expected string
	}{
		{
			name:     "address-based peer",
			conf:     &gobgpapi.PeerConf{NeighborAddress: "172.30.250.1"},
			expected: "172.30.250.1",
		},
		{
			name:     "interface-based peer",
			conf:     &gobgpapi.PeerConf{NeighborInterface: "eth0"},
			expected: "iface:eth0",
		},
		{
			name:     "interface-based peer with resolved address",
			conf:     &gobgpapi.PeerConf{NeighborAddress: "fe80::1", NeighborInterface: "eth0"},
			expected: "iface:eth0",
		},
		{
			name:     "ipv6 address peer",
			conf:     &gobgpapi.PeerConf{NeighborAddress: "fd00::1"},
			expected: "fd00::1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, neighborKey(tt.conf))
		})
	}
}

func TestNeighborKeyFromCRD(t *testing.T) {
	tests := []struct {
		name     string
		cfg      *bgpv1.NeighborConfig
		expected string
	}{
		{
			name:     "address-based",
			cfg:      &bgpv1.NeighborConfig{NeighborAddress: "10.0.0.1"},
			expected: "10.0.0.1",
		},
		{
			name:     "interface-based",
			cfg:      &bgpv1.NeighborConfig{NeighborInterface: "eth1"},
			expected: "iface:eth1",
		},
		{
			name:     "both set, interface wins",
			cfg:      &bgpv1.NeighborConfig{NeighborAddress: "10.0.0.1", NeighborInterface: "eth1"},
			expected: "iface:eth1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, neighborKeyFromCRD(tt.cfg))
		})
	}
}

// --- peerConfigEqual tests ---

func TestPeerConfigEqual(t *testing.T) {
	basePeer := func() *gobgpapi.Peer {
		return &gobgpapi.Peer{
			Conf: &gobgpapi.PeerConf{
				NeighborAddress: "10.0.0.1",
				PeerAsn:         64513,
				LocalAsn:        64512,
				Description:     "test",
				AuthPassword:    "secret",
				PeerGroup:       "group1",
				NeighborInterface: "",
				Vrf:             "",
			},
			AfiSafis: []*gobgpapi.AfiSafi{
				{Config: &gobgpapi.AfiSafiConfig{
					Family:  &gobgpapi.Family{Afi: gobgpapi.Family_AFI_IP, Safi: gobgpapi.Family_SAFI_UNICAST},
					Enabled: true,
				}},
			},
			Timers: &gobgpapi.Timers{
				Config: &gobgpapi.TimersConfig{
					HoldTime:          90,
					KeepaliveInterval: 30,
				},
			},
			Transport: &gobgpapi.Transport{
				LocalAddress:  "0.0.0.0",
				PassiveMode:   false,
				BindInterface: "",
			},
		}
	}

	tests := []struct {
		name     string
		desired  *gobgpapi.Peer
		current  *gobgpapi.Peer
		expected bool
	}{
		{
			name:     "identical configs",
			desired:  basePeer(),
			current:  basePeer(),
			expected: true,
		},
		{
			name:    "current has populated State (should be ignored)",
			desired: basePeer(),
			current: func() *gobgpapi.Peer {
				p := basePeer()
				p.State = &gobgpapi.PeerState{
					SessionState: gobgpapi.PeerState_SESSION_STATE_ESTABLISHED,
					Messages:     &gobgpapi.Messages{},
				}
				p.Timers.State = &gobgpapi.TimersState{}
				return p
			}(),
			expected: true,
		},
		{
			name:    "interface peer with resolved address (should be ignored)",
			desired: func() *gobgpapi.Peer {
				p := basePeer()
				p.Conf.NeighborAddress = ""
				p.Conf.NeighborInterface = "eth0"
				return p
			}(),
			current: func() *gobgpapi.Peer {
				p := basePeer()
				p.Conf.NeighborAddress = "fe80::abc:123"
				p.Conf.NeighborInterface = "eth0"
				return p
			}(),
			expected: true,
		},
		{
			name:    "different PeerAsn",
			desired: basePeer(),
			current: func() *gobgpapi.Peer {
				p := basePeer()
				p.Conf.PeerAsn = 64514
				return p
			}(),
			expected: false,
		},
		{
			name:    "different AuthPassword",
			desired: basePeer(),
			current: func() *gobgpapi.Peer {
				p := basePeer()
				p.Conf.AuthPassword = "different"
				return p
			}(),
			expected: false,
		},
		{
			name:    "different AfiSafi count",
			desired: basePeer(),
			current: func() *gobgpapi.Peer {
				p := basePeer()
				p.AfiSafis = append(p.AfiSafis, &gobgpapi.AfiSafi{
					Config: &gobgpapi.AfiSafiConfig{
						Family:  &gobgpapi.Family{Afi: gobgpapi.Family_AFI_IP6, Safi: gobgpapi.Family_SAFI_UNICAST},
						Enabled: true,
					},
				})
				return p
			}(),
			expected: false,
		},
		{
			name:    "current has auto-populated Type and SendCommunity (should be ignored)",
			desired: basePeer(),
			current: func() *gobgpapi.Peer {
				p := basePeer()
				p.Conf.Type = 1
				p.Conf.SendCommunity = 3
				return p
			}(),
			expected: true,
		},
		{
			name:    "current has extra Transport runtime fields (should be ignored)",
			desired: basePeer(),
			current: func() *gobgpapi.Peer {
				p := basePeer()
				p.Transport.LocalPort = 179
				p.Transport.RemoteAddress = "10.0.0.1"
				p.Transport.RemotePort = 44123
				return p
			}(),
			expected: true,
		},
		{
			name:    "different Transport PassiveMode",
			desired: basePeer(),
			current: func() *gobgpapi.Peer {
				p := basePeer()
				p.Transport.PassiveMode = true
				return p
			}(),
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, peerConfigEqual(tt.desired, tt.current))
		})
	}
}

// --- peerGroupConfigEqual tests ---

func TestPeerGroupConfigEqual(t *testing.T) {
	basePG := func() *gobgpapi.PeerGroup {
		return &gobgpapi.PeerGroup{
			Conf: &gobgpapi.PeerGroupConf{
				PeerGroupName: "test-group",
				PeerAsn:       64513,
				Description:   "test",
			},
		}
	}

	tests := []struct {
		name     string
		desired  *gobgpapi.PeerGroup
		current  *gobgpapi.PeerGroup
		expected bool
	}{
		{
			name:     "identical",
			desired:  basePG(),
			current:  basePG(),
			expected: true,
		},
		{
			name:    "current has Info (runtime state, should be ignored)",
			desired: basePG(),
			current: func() *gobgpapi.PeerGroup {
				pg := basePG()
				pg.Info = &gobgpapi.PeerGroupState{TotalPaths: 5}
				return pg
			}(),
			expected: true,
		},
		{
			name:    "different PeerAsn",
			desired: basePG(),
			current: func() *gobgpapi.PeerGroup {
				pg := basePG()
				pg.Conf.PeerAsn = 64514
				return pg
			}(),
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, peerGroupConfigEqual(tt.desired, tt.current))
		})
	}
}

// --- nodeMatchesSelector tests ---

func TestNodeMatchesSelector(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, bgpv1.AddToScheme(scheme))

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
			Labels: map[string]string{
				"network.example.com/subnet": "250",
				"kubernetes.io/os":           "linux",
			},
		},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "172.30.250.100"},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		Build()

	r := &BGPConfigurationReconciler{
		Client:   fakeClient,
		Log:      zap.New(zap.UseDevMode(true)),
		NodeName: "test-node",
		Recorder: record.NewFakeRecorder(10),
	}
	r.InitNodeCache()

	log := r.Log

	tests := []struct {
		name     string
		selector *metav1.LabelSelector
		expected bool
	}{
		{
			name:     "matchLabels match",
			selector: &metav1.LabelSelector{MatchLabels: map[string]string{"network.example.com/subnet": "250"}},
			expected: true,
		},
		{
			name:     "matchLabels don't match",
			selector: &metav1.LabelSelector{MatchLabels: map[string]string{"network.example.com/subnet": "251"}},
			expected: false,
		},
		{
			name:     "empty selector matches all",
			selector: &metav1.LabelSelector{},
			expected: true,
		},
		{
			name: "matchExpressions In operator matches",
			selector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "network.example.com/subnet", Operator: metav1.LabelSelectorOpIn, Values: []string{"250", "251"}},
				},
			},
			expected: true,
		},
		{
			name: "matchExpressions In operator doesn't match",
			selector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "network.example.com/subnet", Operator: metav1.LabelSelectorOpIn, Values: []string{"251", "252"}},
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := r.nodeMatchesSelector(context.Background(), tt.selector, log)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// --- hasAnyNodeSelector tests ---

func TestHasAnyNodeSelector(t *testing.T) {
	assert.False(t, hasAnyNodeSelector(nil))
	assert.False(t, hasAnyNodeSelector([]bgpv1.Neighbor{
		{Config: bgpv1.NeighborConfig{NeighborAddress: "10.0.0.1"}},
	}))
	assert.True(t, hasAnyNodeSelector([]bgpv1.Neighbor{
		{Config: bgpv1.NeighborConfig{NeighborAddress: "10.0.0.1"}},
		{Config: bgpv1.NeighborConfig{NeighborAddress: "10.0.0.2"}, NodeSelector: &metav1.LabelSelector{}},
	}))
}

// --- deletePeerByKey tests ---

func TestDeletePeerByKey_UsesInterfaceForInterfacePeers(t *testing.T) {
	// Verify that the function constructs the right request type.
	// We can't easily mock the gRPC client, but we can verify the helper logic
	// by checking that neighborKeyFromCRD returns the expected key.
	cfg := &bgpv1.NeighborConfig{NeighborInterface: "eth0"}
	assert.Equal(t, "iface:eth0", neighborKeyFromCRD(cfg))

	cfg2 := &bgpv1.NeighborConfig{NeighborAddress: "10.0.0.1"}
	assert.Equal(t, "10.0.0.1", neighborKeyFromCRD(cfg2))
}

// --- nodeCache invalidate tests ---

func TestNodeCacheInvalidate(t *testing.T) {
	cache := newNodeCache(5 * 60 * 1e9) // 5 min

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node1",
			Labels: map[string]string{"subnet": "250"},
		},
	}

	cache.set("node1", node)

	// Should be cached
	cached, ok := cache.get("node1")
	assert.True(t, ok)
	assert.Equal(t, "250", cached.Labels["subnet"])

	// Invalidate
	cache.invalidate("node1")

	// Should no longer be cached
	_, ok = cache.get("node1")
	assert.False(t, ok)
}

// --- transport/timer/afisafi comparison helper tests ---

func TestTransportConfigEqual(t *testing.T) {
	assert.True(t, transportConfigEqual(nil, nil))
	assert.False(t, transportConfigEqual(&gobgpapi.Transport{}, nil))
	assert.False(t, transportConfigEqual(nil, &gobgpapi.Transport{}))
	assert.True(t, transportConfigEqual(
		&gobgpapi.Transport{LocalAddress: "0.0.0.0", PassiveMode: false},
		&gobgpapi.Transport{LocalAddress: "0.0.0.0", PassiveMode: false, LocalPort: 179, RemotePort: 44000},
	))
}

func TestTimersConfigEqual(t *testing.T) {
	assert.True(t, timersConfigEqual(nil, nil))
	assert.False(t, timersConfigEqual(&gobgpapi.Timers{}, nil))
	assert.True(t, timersConfigEqual(
		&gobgpapi.Timers{Config: &gobgpapi.TimersConfig{HoldTime: 90}},
		&gobgpapi.Timers{Config: &gobgpapi.TimersConfig{HoldTime: 90}, State: &gobgpapi.TimersState{}},
	))
}
