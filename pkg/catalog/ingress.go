package catalog

import (
	"fmt"
	"regexp"
	"strings"

	networkingV1beta1 "k8s.io/api/networking/v1beta1"

	"github.com/openservicemesh/osm/pkg/constants"
	"github.com/openservicemesh/osm/pkg/service"
	"github.com/openservicemesh/osm/pkg/trafficpolicy"
)

const (
	// prefixMatchPathElementsRegex is the regex pattern used to match zero or one grouping of path elements.
	// A path element is of the form /p, /p1/p2, /p1/p2/p3 etc.
	// This regex pattern is used to match paths in a way that is compatible with Kubernetes ingress requirements
	// for Prefix path type, where the prefix must be an element wise prefix and not a string prefix.
	// Ref: https://kubernetes.io/docs/concepts/services-networking/ingress/#path-types
	// It is used to regex match paths such that request /foo matches /foo and /foo/bar, but not /foobar.
	prefixMatchPathElementsRegex = `(/.*)?$`

	// commonRegexChars is a string comprising of characters commonly used in a regex
	// It is used to guess whether a path specified appears as a regex.
	// It is used as a fallback to match ingress paths whose PathType is set to be ImplementationSpecific.
	commonRegexChars = `^$*+[]%|`
)

// Ensure the regex patteren for prefix matching for path elements compiles
var _ = regexp.MustCompile(prefixMatchPathElementsRegex)

// Ingress does not depend on k8s service accounts, program a wildcard (empty struct) to indicate
// to RDS that an inbound traffic policy for ingress should not enforce service account based RBAC policies.
var wildcardServiceAccount = service.K8sServiceAccount{}

// GetIngressPoliciesForService returns a list of inbound traffic policies for a service as defined in observed ingress k8s resources.
func (mc *MeshCatalog) GetIngressPoliciesForService(svc service.MeshService) ([]*trafficpolicy.InboundTrafficPolicy, error) {
	inboundIngressPolicies := []*trafficpolicy.InboundTrafficPolicy{}

	ingresses, err := mc.ingressMonitor.GetIngressResources(svc)
	if err != nil {
		log.Error().Err(err).Msgf("Failed to get ingress resources for service %s", svc)
		return inboundIngressPolicies, err
	}
	if len(ingresses) == 0 {
		log.Trace().Msgf("No ingress resources found for service %s", svc)
		return inboundIngressPolicies, err
	}

	ingressWeightedCluster := getDefaultWeightedClusterForService(svc)

	for _, ingress := range ingresses {
		if ingress.Spec.Backend != nil && ingress.Spec.Backend.ServiceName == svc.Name {
			wildcardIngressPolicy := trafficpolicy.NewInboundTrafficPolicy(buildIngressPolicyName(ingress.ObjectMeta.Name, ingress.ObjectMeta.Namespace, constants.WildcardHTTPMethod), []string{constants.WildcardHTTPMethod})
			wildcardIngressPolicy.AddRule(*trafficpolicy.NewRouteWeightedCluster(trafficpolicy.WildCardRouteMatch, []service.WeightedCluster{ingressWeightedCluster}), wildcardServiceAccount)
			inboundIngressPolicies = trafficpolicy.MergeInboundPolicies(false, inboundIngressPolicies, wildcardIngressPolicy)
		}

		for _, rule := range ingress.Spec.Rules {
			domain := rule.Host
			if domain == "" {
				domain = constants.WildcardHTTPMethod
			}
			ingressPolicy := trafficpolicy.NewInboundTrafficPolicy(buildIngressPolicyName(ingress.ObjectMeta.Name, ingress.ObjectMeta.Namespace, domain), []string{domain})

			for _, ingressPath := range rule.HTTP.Paths {
				if ingressPath.Backend.ServiceName != svc.Name {
					continue
				}

				httpRouteMatch := trafficpolicy.HTTPRouteMatch{
					Methods: []string{constants.WildcardHTTPMethod},
				}

				// Default ingress path type to PathTypeImplementationSpecific if unspecified
				pathType := networkingV1beta1.PathTypeImplementationSpecific
				if ingressPath.PathType != nil {
					pathType = *ingressPath.PathType
				}

				switch pathType {
				case networkingV1beta1.PathTypeExact:
					// Exact match
					// Request /foo matches path /foo, not /foobar or /foo/bar
					httpRouteMatch.Path = ingressPath.Path
					httpRouteMatch.PathMatchType = trafficpolicy.PathMatchExact

				case networkingV1beta1.PathTypePrefix:
					// Element wise prefix match
					// Request /foo matches path /foo and /foo/bar, not /foobar
					httpRouteMatch.Path = ingressPath.Path + prefixMatchPathElementsRegex
					httpRouteMatch.PathMatchType = trafficpolicy.PathMatchRegex

				case networkingV1beta1.PathTypeImplementationSpecific:
					httpRouteMatch.Path = ingressPath.Path
					// If the path looks like a regex, use regex matching.
					// Else use string based prefix matching.
					if strings.ContainsAny(ingressPath.Path, commonRegexChars) {
						// Path contains regex characters, use regex matching for the path
						// Request /foo/bar matches path /foo.*
						httpRouteMatch.PathMatchType = trafficpolicy.PathMatchRegex
					} else {
						// String based prefix path matching
						// Request /foo matches /foo/bar and /foobar
						httpRouteMatch.PathMatchType = trafficpolicy.PathMatchPrefix
					}

				default:
					log.Error().Msgf("Invalid pathType=%s unspecified for path %s in ingress resource %s/%s, ignoring this path", *ingressPath.PathType, ingressPath.Path, ingress.Namespace, ingress.Name)
					continue
				}

				ingressPolicy.AddRule(*trafficpolicy.NewRouteWeightedCluster(httpRouteMatch, []service.WeightedCluster{ingressWeightedCluster}), wildcardServiceAccount)
			}

			// Only create an ingress policy if the ingress policy resulted in valid rules
			if len(ingressPolicy.Rules) > 0 {
				inboundIngressPolicies = trafficpolicy.MergeInboundPolicies(false, inboundIngressPolicies, ingressPolicy)
			}
		}
	}
	return inboundIngressPolicies, nil
}

func buildIngressPolicyName(name, namespace, host string) string {
	policyName := fmt.Sprintf("%s.%s|%s", name, namespace, host)
	return policyName
}
