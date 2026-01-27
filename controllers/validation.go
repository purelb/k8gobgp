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
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
)

// validateIPAddress validates IP address format
func validateIPAddress(addr string) error {
	ip := net.ParseIP(addr)
	if ip == nil {
		return fmt.Errorf("invalid IP address: %s", addr)
	}
	return nil
}

// validateMetricValue caps metric values to reasonable bounds
func validateMetricValue(value uint64) uint64 {
	const maxMetricValue = 1e9 // 1 billion routes is a reasonable upper bound
	if value > uint64(maxMetricValue) {
		return uint64(maxMetricValue)
	}
	return value
}

// sanitizeLabelValue ensures label values are safe for Prometheus
func sanitizeLabelValue(value string) string {
	// Remove characters that break Prometheus format
	value = strings.ReplaceAll(value, "\n", "_")
	value = strings.ReplaceAll(value, "\r", "_")
	value = strings.ReplaceAll(value, "\"", "_")
	value = strings.ReplaceAll(value, "\\", "_")

	// Limit length to prevent resource exhaustion
	const maxLabelLength = 128
	if len(value) > maxLabelLength {
		value = value[:maxLabelLength]
	}

	return value
}

// sanitizeFamilyString validates family string against whitelist
func sanitizeFamilyString(family string) string {
	validFamilies := map[string]bool{
		"ipv4_unicast":          true,
		"ipv6_unicast":          true,
		"l2vpn_evpn":            true,
		"ipv4_multicast":        true,
		"ipv6_multicast":        true,
		"ipv4_mpls":             true,
		"ipv6_mpls":             true,
		"ipv4_flowspec":         true,
		"ipv6_flowspec":         true,
		"ipv4_flowspec_unicast": true,
		"ipv6_flowspec_unicast": true,
	}

	if validFamilies[family] {
		return family
	}
	return "unknown"
}

// communityPattern matches standard BGP community format "AS:VALUE"
var communityPattern = regexp.MustCompile(`^\d{1,5}:\d{1,5}$`)

// largeCommunityPattern matches large BGP community format "ASN:LocalData1:LocalData2"
var largeCommunityPattern = regexp.MustCompile(`^\d+:\d+:\d+$`)

// ValidationError represents a configuration validation error
type ValidationError struct {
	Field   string
	Value   string
	Message string
}

func (e ValidationError) Error() string {
	return fmt.Sprintf("%s: %s (%s)", e.Field, e.Message, e.Value)
}

// ValidationResult holds the results of configuration validation
type ValidationResult struct {
	Valid  bool
	Errors []ValidationError
}

// AddError adds an error to the validation result
func (v *ValidationResult) AddError(field, value, message string) {
	v.Valid = false
	v.Errors = append(v.Errors, ValidationError{Field: field, Value: value, Message: message})
}

// ErrorMessages returns all error messages as a single string
func (v *ValidationResult) ErrorMessages() string {
	if len(v.Errors) == 0 {
		return ""
	}
	messages := make([]string, len(v.Errors))
	for i, e := range v.Errors {
		messages[i] = e.Error()
	}
	return strings.Join(messages, "; ")
}

// ValidateCommunity validates a standard BGP community string (format: "AS:VALUE")
// where AS and VALUE are both 16-bit unsigned integers (0-65535)
func ValidateCommunity(community string) error {
	if !communityPattern.MatchString(community) {
		return fmt.Errorf("invalid community format: %q (expected AS:VALUE where AS and VALUE are numbers)", community)
	}

	// Parse and validate the numeric values
	parts := strings.Split(community, ":")
	asn, err := strconv.ParseUint(parts[0], 10, 16)
	if err != nil || asn > 65535 {
		return fmt.Errorf("invalid AS number in community %q: must be 0-65535", community)
	}
	value, err := strconv.ParseUint(parts[1], 10, 16)
	if err != nil || value > 65535 {
		return fmt.Errorf("invalid value in community %q: must be 0-65535", community)
	}

	return nil
}

// ValidateLargeCommunity validates a large BGP community string (format: "ASN:LocalData1:LocalData2")
// where all values are 32-bit unsigned integers
func ValidateLargeCommunity(community string) error {
	if !largeCommunityPattern.MatchString(community) {
		return fmt.Errorf("invalid large community format: %q (expected ASN:LocalData1:LocalData2)", community)
	}

	// Parse and validate the numeric values (all 32-bit)
	parts := strings.Split(community, ":")
	for i, part := range parts {
		val, err := strconv.ParseUint(part, 10, 32)
		if err != nil || val > 4294967295 {
			return fmt.Errorf("invalid value at position %d in large community %q: must be 0-4294967295", i+1, community)
		}
	}

	return nil
}
