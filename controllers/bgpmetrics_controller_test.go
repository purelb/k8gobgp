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
	"strings"
	"testing"

	gobgpapi "github.com/osrg/gobgp/v4/api"
	"github.com/stretchr/testify/assert"
)

func TestFamilyToString(t *testing.T) {
	tests := []struct {
		name     string
		family   *gobgpapi.Family
		expected string
	}{
		{
			name:     "nil family",
			family:   nil,
			expected: "unknown",
		},
		{
			name: "ipv4 unicast",
			family: &gobgpapi.Family{
				Afi:  gobgpapi.Family_AFI_IP,
				Safi: gobgpapi.Family_SAFI_UNICAST,
			},
			expected: "ipv4_unicast",
		},
		{
			name: "ipv6 unicast",
			family: &gobgpapi.Family{
				Afi:  gobgpapi.Family_AFI_IP6,
				Safi: gobgpapi.Family_SAFI_UNICAST,
			},
			expected: "ipv6_unicast",
		},
		{
			name: "l2vpn evpn",
			family: &gobgpapi.Family{
				Afi:  gobgpapi.Family_AFI_L2VPN,
				Safi: gobgpapi.Family_SAFI_EVPN,
			},
			expected: "l2vpn_evpn",
		},
		{
			name: "ipv4 multicast",
			family: &gobgpapi.Family{
				Afi:  gobgpapi.Family_AFI_IP,
				Safi: gobgpapi.Family_SAFI_MULTICAST,
			},
			expected: "ipv4_multicast",
		},
		{
			name: "ipv6 flowspec unicast",
			family: &gobgpapi.Family{
				Afi:  gobgpapi.Family_AFI_IP6,
				Safi: gobgpapi.Family_SAFI_FLOW_SPEC_UNICAST,
			},
			expected: "ipv6_flowspec_unicast",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := familyToString(tt.family)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestSanitizeNeighborAddress(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"valid ipv4", "192.168.1.1", "192.168.1.1"},
		{"valid ipv6", "2001:db8::1", "2001:db8::1"},
		{"valid ipv6 full", "2001:0db8:0000:0000:0000:0000:0000:0001", "2001:db8::1"},
		{"invalid", "not-an-ip", "invalid"},
		{"injection attempt", `192.168.1.1",family="bad`, "invalid"},
		{"empty", "", "invalid"},
		{"partial ip", "192.168.1", "invalid"},
		{"localhost ipv4", "127.0.0.1", "127.0.0.1"},
		{"localhost ipv6", "::1", "::1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sanitizeNeighborAddress(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestDefaultFamilies(t *testing.T) {
	families := defaultFamilies()

	assert.Len(t, families, 2)

	// Check IPv4 unicast
	assert.Equal(t, gobgpapi.Family_AFI_IP, families[0].Afi)
	assert.Equal(t, gobgpapi.Family_SAFI_UNICAST, families[0].Safi)

	// Check IPv6 unicast
	assert.Equal(t, gobgpapi.Family_AFI_IP6, families[1].Afi)
	assert.Equal(t, gobgpapi.Family_SAFI_UNICAST, families[1].Safi)
}

func TestValidateIPAddress(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid ipv4", "192.168.1.1", false},
		{"valid ipv6", "2001:db8::1", false},
		{"invalid", "not-an-ip", true},
		{"empty", "", true},
		{"partial", "192.168", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateIPAddress(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestValidateMetricValue(t *testing.T) {
	tests := []struct {
		name     string
		input    uint64
		expected uint64
	}{
		{"normal value", 100, 100},
		{"zero", 0, 0},
		{"large but valid", 999999999, 999999999},
		{"at max", 1000000000, 1000000000},
		{"over max", 2000000000, 1000000000},
		{"way over max", 9999999999999, 1000000000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := validateMetricValue(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestSanitizeLabelValue(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"normal", "test", "test"},
		{"with spaces", "test value", "test value"},
		{"newline", "test\nvalue", "test_value"},
		{"carriage return", "test\rvalue", "test_value"},
		{"quote", `test"value`, "test_value"},
		{"backslash", `test\value`, "test_value"},
		{"multiple special", "test\n\r\"\\value", "test____value"},
		{"long string", strings.Repeat("a", 200), strings.Repeat("a", 128)},
		{"empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sanitizeLabelValue(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestSanitizeFamilyString(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"ipv4 unicast", "ipv4_unicast", "ipv4_unicast"},
		{"ipv6 unicast", "ipv6_unicast", "ipv6_unicast"},
		{"l2vpn evpn", "l2vpn_evpn", "l2vpn_evpn"},
		{"ipv4 multicast", "ipv4_multicast", "ipv4_multicast"},
		{"ipv6 flowspec", "ipv6_flowspec_unicast", "ipv6_flowspec_unicast"},
		{"unknown family", "random_family", "unknown"},
		{"empty", "", "unknown"},
		{"injection attempt", "ipv4_unicast\"; DROP TABLE", "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sanitizeFamilyString(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestMetricsConfig_Defaults(t *testing.T) {
	config := MetricsConfig{}

	// Zero values should be handled by the controller
	assert.Equal(t, 0, config.MaxNeighborsForMetrics)
	assert.False(t, config.EnablePerNeighborMetrics)
	assert.Equal(t, 0*0, int(config.PollInterval.Seconds()))
}

func TestValidateCommunity(t *testing.T) {
	tests := []struct {
		name      string
		community string
		wantErr   bool
	}{
		{"valid standard", "65000:100", false},
		{"valid zero values", "0:0", false},
		{"valid max values", "65535:65535", false},
		{"valid short numbers", "1:1", false},
		{"invalid format - no colon", "65000100", true},
		{"invalid format - text", "invalid-community", true},
		{"invalid format - too many colons", "65000:100:200", true},
		{"invalid AS - too large", "65536:100", true},
		{"invalid value - too large", "65000:65536", true},
		{"invalid AS - negative", "-1:100", true},
		{"invalid - empty", "", true},
		{"invalid - spaces", "65000: 100", true},
		{"invalid - letters in AS", "AS100:100", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateCommunity(tt.community)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestValidateLargeCommunity(t *testing.T) {
	tests := []struct {
		name      string
		community string
		wantErr   bool
	}{
		{"valid standard", "65000:100:200", false},
		{"valid zero values", "0:0:0", false},
		{"valid max values", "4294967295:4294967295:4294967295", false},
		{"valid short numbers", "1:2:3", false},
		{"invalid format - no colons", "65000100200", true},
		{"invalid format - text", "invalid-large-community", true},
		{"invalid format - only two parts", "65000:100", true},
		{"invalid format - four parts", "65000:100:200:300", true},
		{"invalid - empty", "", true},
		{"invalid - spaces", "65000: 100:200", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateLargeCommunity(tt.community)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestValidationResult(t *testing.T) {
	t.Run("new result is valid", func(t *testing.T) {
		result := &ValidationResult{Valid: true}
		assert.True(t, result.Valid)
		assert.Empty(t, result.Errors)
	})

	t.Run("add error makes invalid", func(t *testing.T) {
		result := &ValidationResult{Valid: true}
		result.AddError("field", "value", "error message")
		assert.False(t, result.Valid)
		assert.Len(t, result.Errors, 1)
		assert.Equal(t, "field", result.Errors[0].Field)
		assert.Equal(t, "value", result.Errors[0].Value)
		assert.Equal(t, "error message", result.Errors[0].Message)
	})

	t.Run("error messages formatting", func(t *testing.T) {
		result := &ValidationResult{Valid: true}
		assert.Empty(t, result.ErrorMessages())

		result.AddError("field1", "val1", "msg1")
		result.AddError("field2", "val2", "msg2")
		messages := result.ErrorMessages()
		assert.Contains(t, messages, "field1")
		assert.Contains(t, messages, "field2")
		assert.Contains(t, messages, "; ")
	})
}
