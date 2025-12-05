package v1

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var bgpconfigurationlog = logf.Log.WithName("bgpconfiguration-resource")

// BGPConfigurationCustomValidator implements CustomValidator for BGPConfiguration
type BGPConfigurationCustomValidator struct{}

func (r *BGPConfiguration) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(r).
		WithValidator(&BGPConfigurationCustomValidator{}).
		Complete()
}

// +kubebuilder:webhook:path=/validate-bgp-purelb-io-v1-bgpconfiguration,mutating=false,failurePolicy=fail,sideEffects=None,groups=bgp.purelb.io,resources=bgpconfigurations,verbs=create;update,versions=v1,name=vbgpconfiguration.kb.io,admissionReviewVersions=v1

var _ webhook.CustomValidator = &BGPConfigurationCustomValidator{}

// ValidateCreate implements webhook.CustomValidator
func (v *BGPConfigurationCustomValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	r, ok := obj.(*BGPConfiguration)
	if !ok {
		return nil, fmt.Errorf("expected BGPConfiguration, got %T", obj)
	}
	bgpconfigurationlog.Info("validate create", "name", r.Name)
	return r.validateBGPConfiguration()
}

// ValidateUpdate implements webhook.CustomValidator
func (v *BGPConfigurationCustomValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	r, ok := newObj.(*BGPConfiguration)
	if !ok {
		return nil, fmt.Errorf("expected BGPConfiguration, got %T", newObj)
	}
	bgpconfigurationlog.Info("validate update", "name", r.Name)
	return r.validateBGPConfiguration()
}

// ValidateDelete implements webhook.CustomValidator
func (v *BGPConfigurationCustomValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	r, ok := obj.(*BGPConfiguration)
	if !ok {
		return nil, fmt.Errorf("expected BGPConfiguration, got %T", obj)
	}
	bgpconfigurationlog.Info("validate delete", "name", r.Name)
	// No validation needed for delete
	return nil, nil
}

func (r *BGPConfiguration) validateBGPConfiguration() (admission.Warnings, error) {
	var allErrs field.ErrorList
	var warnings admission.Warnings

	// Validate global configuration
	globalErrs := r.validateGlobalSpec()
	allErrs = append(allErrs, globalErrs...)

	// Validate neighbors
	neighborsPath := field.NewPath("spec").Child("neighbors")
	for i, neighbor := range r.Spec.Neighbors {
		neighborPath := neighborsPath.Index(i)
		neighborErrs, neighborWarnings := r.validateNeighbor(&neighbor, neighborPath)
		allErrs = append(allErrs, neighborErrs...)
		warnings = append(warnings, neighborWarnings...)
	}

	// Validate peer groups
	peerGroupsPath := field.NewPath("spec").Child("peerGroups")
	for i, pg := range r.Spec.PeerGroups {
		pgPath := peerGroupsPath.Index(i)
		pgErrs, pgWarnings := r.validatePeerGroup(&pg, pgPath)
		allErrs = append(allErrs, pgErrs...)
		warnings = append(warnings, pgWarnings...)
	}

	// Validate VRFs
	vrfsPath := field.NewPath("spec").Child("vrfs")
	for i, vrf := range r.Spec.Vrfs {
		vrfPath := vrfsPath.Index(i)
		vrfErrs := r.validateVrf(&vrf, vrfPath)
		allErrs = append(allErrs, vrfErrs...)
	}

	// Validate dynamic neighbors
	dynNeighborsPath := field.NewPath("spec").Child("dynamicNeighbors")
	for i, dn := range r.Spec.DynamicNeighbors {
		dnPath := dynNeighborsPath.Index(i)
		dnErrs := r.validateDynamicNeighbor(&dn, dnPath)
		allErrs = append(allErrs, dnErrs...)
	}

	// Validate policies
	policiesPath := field.NewPath("spec").Child("policyDefinitions")
	for i, policy := range r.Spec.PolicyDefinitions {
		policyPath := policiesPath.Index(i)
		policyErrs := r.validatePolicy(&policy, policyPath)
		allErrs = append(allErrs, policyErrs...)
	}

	// Validate netlink configuration
	netlinkErrs := r.validateNetlinkConfig()
	allErrs = append(allErrs, netlinkErrs...)

	if len(allErrs) == 0 {
		return warnings, nil
	}
	return warnings, allErrs.ToAggregate()
}

func (r *BGPConfiguration) validateGlobalSpec() field.ErrorList {
	var errs field.ErrorList
	globalPath := field.NewPath("spec").Child("global")

	// Validate ASN (must be > 0 and <= 4294967295)
	if r.Spec.Global.ASN == 0 {
		errs = append(errs, field.Required(globalPath.Child("asn"), "ASN is required"))
	}

	// Validate Router ID (must be valid IPv4)
	if r.Spec.Global.RouterID != "" {
		if ip := net.ParseIP(r.Spec.Global.RouterID); ip == nil || ip.To4() == nil {
			errs = append(errs, field.Invalid(globalPath.Child("routerID"), r.Spec.Global.RouterID, "must be a valid IPv4 address"))
		}
	}

	// Validate listen port
	if r.Spec.Global.ListenPort < 0 || r.Spec.Global.ListenPort > 65535 {
		errs = append(errs, field.Invalid(globalPath.Child("listenPort"), r.Spec.Global.ListenPort, "must be between 0 and 65535"))
	}

	// Validate listen addresses
	for i, addr := range r.Spec.Global.ListenAddresses {
		if net.ParseIP(addr) == nil {
			errs = append(errs, field.Invalid(globalPath.Child("listenAddresses").Index(i), addr, "must be a valid IP address"))
		}
	}

	// Validate address families
	validFamilies := map[string]bool{
		"ipv4-unicast": true,
		"ipv6-unicast": true,
		"l2vpn-evpn":   true,
		"ipv4-mpls":    true,
		"ipv6-mpls":    true,
	}
	for i, family := range r.Spec.Global.Families {
		if !validFamilies[strings.ToLower(family)] {
			errs = append(errs, field.NotSupported(globalPath.Child("families").Index(i), family, []string{"ipv4-unicast", "ipv6-unicast", "l2vpn-evpn", "ipv4-mpls", "ipv6-mpls"}))
		}
	}

	return errs
}

func (r *BGPConfiguration) validateNeighbor(neighbor *Neighbor, path *field.Path) (field.ErrorList, admission.Warnings) {
	var errs field.ErrorList
	var warnings admission.Warnings
	configPath := path.Child("config")

	// Validate neighbor address
	if neighbor.Config.NeighborAddress == "" {
		errs = append(errs, field.Required(configPath.Child("neighborAddress"), "neighbor address is required"))
	} else if net.ParseIP(neighbor.Config.NeighborAddress) == nil {
		errs = append(errs, field.Invalid(configPath.Child("neighborAddress"), neighbor.Config.NeighborAddress, "must be a valid IP address"))
	}

	// Validate peer ASN
	if neighbor.Config.PeerAsn == 0 {
		errs = append(errs, field.Required(configPath.Child("peerAsn"), "peer ASN is required"))
	}

	// Warn about deprecated inline password
	if neighbor.Config.AuthPassword != "" && neighbor.Config.AuthPasswordSecretRef == nil {
		warnings = append(warnings, fmt.Sprintf("neighbor %s: inline authPassword is deprecated, use authPasswordSecretRef instead", neighbor.Config.NeighborAddress))
	}

	// Validate timers
	if neighbor.Timers != nil {
		timersPath := path.Child("timers").Child("config")
		// Hold time must be >= 3 or 0 (disabled)
		if neighbor.Timers.Config.HoldTime != 0 && neighbor.Timers.Config.HoldTime < 3 {
			errs = append(errs, field.Invalid(timersPath.Child("holdTime"), neighbor.Timers.Config.HoldTime, "must be 0 or >= 3 seconds"))
		}
		// Keepalive must be <= hold time / 3
		if neighbor.Timers.Config.HoldTime > 0 && neighbor.Timers.Config.KeepaliveInterval > neighbor.Timers.Config.HoldTime/3 {
			errs = append(errs, field.Invalid(timersPath.Child("keepaliveInterval"), neighbor.Timers.Config.KeepaliveInterval, "should be <= holdTime/3"))
		}
	}

	// Validate EBGP multihop TTL
	if neighbor.EbgpMultihop != nil && neighbor.EbgpMultihop.Enabled {
		if neighbor.EbgpMultihop.MultihopTtl == 0 || neighbor.EbgpMultihop.MultihopTtl > 255 {
			errs = append(errs, field.Invalid(path.Child("ebgpMultihop").Child("multihopTtl"), neighbor.EbgpMultihop.MultihopTtl, "must be between 1 and 255"))
		}
	}

	// Validate AFI/SAFI
	for i, afiSafi := range neighbor.AfiSafis {
		afiPath := path.Child("afiSafis").Index(i)
		if !isValidFamily(afiSafi.Family) {
			errs = append(errs, field.NotSupported(afiPath.Child("family"), afiSafi.Family, []string{"ipv4-unicast", "ipv6-unicast", "l2vpn-evpn"}))
		}
	}

	return errs, warnings
}

func (r *BGPConfiguration) validatePeerGroup(pg *PeerGroup, path *field.Path) (field.ErrorList, admission.Warnings) {
	var errs field.ErrorList
	var warnings admission.Warnings
	configPath := path.Child("config")

	// Validate peer group name
	if pg.Config.PeerGroupName == "" {
		errs = append(errs, field.Required(configPath.Child("peerGroupName"), "peer group name is required"))
	}

	// Validate name format (alphanumeric and hyphens)
	if !isValidName(pg.Config.PeerGroupName) {
		errs = append(errs, field.Invalid(configPath.Child("peerGroupName"), pg.Config.PeerGroupName, "must contain only alphanumeric characters, hyphens, and underscores"))
	}

	// Warn about deprecated inline password
	if pg.Config.AuthPassword != "" && pg.Config.AuthPasswordSecretRef == nil {
		warnings = append(warnings, fmt.Sprintf("peerGroup %s: inline authPassword is deprecated, use authPasswordSecretRef instead", pg.Config.PeerGroupName))
	}

	return errs, warnings
}

func (r *BGPConfiguration) validateVrf(vrf *Vrf, path *field.Path) field.ErrorList {
	var errs field.ErrorList

	// Validate VRF name
	if vrf.Name == "" {
		errs = append(errs, field.Required(path.Child("name"), "VRF name is required"))
	}

	// Validate route distinguisher format (ASN:NN or IP:NN)
	if vrf.Rd != "" && !isValidRouteDistinguisher(vrf.Rd) {
		errs = append(errs, field.Invalid(path.Child("rd"), vrf.Rd, "must be in format ASN:NN or IP:NN"))
	}

	// Validate route targets
	for i, rt := range vrf.ImportRt {
		if !isValidRouteTarget(rt) {
			errs = append(errs, field.Invalid(path.Child("importRt").Index(i), rt, "must be in format ASN:NN or IP:NN"))
		}
	}
	for i, rt := range vrf.ExportRt {
		if !isValidRouteTarget(rt) {
			errs = append(errs, field.Invalid(path.Child("exportRt").Index(i), rt, "must be in format ASN:NN or IP:NN"))
		}
	}

	return errs
}

func (r *BGPConfiguration) validateDynamicNeighbor(dn *DynamicNeighbor, path *field.Path) field.ErrorList {
	var errs field.ErrorList

	// Validate prefix (must be valid CIDR)
	if dn.Prefix == "" {
		errs = append(errs, field.Required(path.Child("prefix"), "prefix is required"))
	} else if _, _, err := net.ParseCIDR(dn.Prefix); err != nil {
		errs = append(errs, field.Invalid(path.Child("prefix"), dn.Prefix, "must be a valid CIDR notation"))
	}

	// Validate peer group reference
	if dn.PeerGroup == "" {
		errs = append(errs, field.Required(path.Child("peerGroup"), "peer group is required for dynamic neighbors"))
	} else {
		// Verify peer group exists
		found := false
		for _, pg := range r.Spec.PeerGroups {
			if pg.Config.PeerGroupName == dn.PeerGroup {
				found = true
				break
			}
		}
		if !found {
			errs = append(errs, field.NotFound(path.Child("peerGroup"), dn.PeerGroup))
		}
	}

	return errs
}

func (r *BGPConfiguration) validatePolicy(policy *PolicyDefinition, path *field.Path) field.ErrorList {
	var errs field.ErrorList

	// Validate policy name
	if policy.Name == "" {
		errs = append(errs, field.Required(path.Child("name"), "policy name is required"))
	}

	// Validate statements
	for i, stmt := range policy.Statements {
		stmtPath := path.Child("statements").Index(i)
		if stmt.Name == "" {
			errs = append(errs, field.Required(stmtPath.Child("name"), "statement name is required"))
		}

		// Validate route action
		validActions := []string{"accept", "reject", ""}
		if !contains(validActions, strings.ToLower(stmt.Actions.RouteAction)) {
			errs = append(errs, field.NotSupported(stmtPath.Child("actions").Child("routeAction"), stmt.Actions.RouteAction, []string{"accept", "reject"}))
		}
	}

	return errs
}

func (r *BGPConfiguration) validateNetlinkConfig() field.ErrorList {
	var errs field.ErrorList

	// Validate netlink export rules
	if r.Spec.NetlinkExport != nil && r.Spec.NetlinkExport.Enabled {
		exportPath := field.NewPath("spec").Child("netlinkExport")

		// Validate route protocol (should be within valid range)
		if r.Spec.NetlinkExport.RouteProtocol < 0 || r.Spec.NetlinkExport.RouteProtocol > 255 {
			errs = append(errs, field.Invalid(exportPath.Child("routeProtocol"), r.Spec.NetlinkExport.RouteProtocol, "must be between 0 and 255"))
		}

		for i, rule := range r.Spec.NetlinkExport.Rules {
			rulePath := exportPath.Child("rules").Index(i)
			if rule.Name == "" {
				errs = append(errs, field.Required(rulePath.Child("name"), "rule name is required"))
			}
		}
	}

	return errs
}

// Helper functions

func isValidFamily(family string) bool {
	validFamilies := map[string]bool{
		"ipv4-unicast": true,
		"ipv6-unicast": true,
		"l2vpn-evpn":   true,
		"ipv4-mpls":    true,
		"ipv6-mpls":    true,
	}
	return validFamilies[strings.ToLower(family)]
}

func isValidName(name string) bool {
	if name == "" {
		return false
	}
	// Allow alphanumeric, hyphens, and underscores
	match, _ := regexp.MatchString(`^[a-zA-Z0-9_-]+$`, name)
	return match
}

func isValidRouteDistinguisher(rd string) bool {
	// Format: ASN:NN or IP:NN
	parts := strings.Split(rd, ":")
	if len(parts) != 2 {
		return false
	}
	// Check if first part is IP or ASN
	if net.ParseIP(parts[0]) != nil {
		return true
	}
	// Otherwise should be a number (ASN)
	var asn uint32
	_, err := fmt.Sscanf(parts[0], "%d", &asn)
	return err == nil
}

func isValidRouteTarget(rt string) bool {
	// Same format as route distinguisher
	return isValidRouteDistinguisher(rt)
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}
