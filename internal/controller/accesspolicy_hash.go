package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	cfztv1alpha1 "github.com/andrewreid/cfzt-operator/api/v1alpha1"
	"github.com/andrewreid/cfzt-operator/internal/cloudflare"
)

type canonicalAccessRules struct {
	Include []canonicalAccessRule `json:"include,omitempty"`
	Exclude []canonicalAccessRule `json:"exclude,omitempty"`
	Require []canonicalAccessRule `json:"require,omitempty"`
}

type canonicalAccessRule struct {
	Email          string `json:"email,omitempty"`
	EmailDomain    string `json:"emailDomain,omitempty"`
	Everyone       bool   `json:"everyone,omitempty"`
	GeoCountryCode string `json:"geoCountryCode,omitempty"`
	IP             string `json:"ip,omitempty"`
	ServiceToken   string `json:"serviceToken,omitempty"`
}

func accessPolicyRulesHash(policy *cfztv1alpha1.CloudflareAccessPolicy) (string, error) {
	canonical := canonicalAccessRules{
		Include: canonicalize(translateRules(policy.Spec.Rules.Include)),
		Exclude: canonicalize(translateRules(policy.Spec.Rules.Exclude)),
		Require: canonicalize(translateRules(policy.Spec.Rules.Require)),
	}
	b, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// translateRules converts api/v1alpha1 rules to the internal cloudflare shape.
// Both sides are discriminated unions with exactly one field set per item (CEL
// guarantees this on the API side); a straight copy is sufficient.
func translateRules(in []cfztv1alpha1.AccessRule) []cloudflare.AccessRule {
	if len(in) == 0 {
		return nil
	}
	out := make([]cloudflare.AccessRule, 0, len(in))
	for _, r := range in {
		out = append(out, cloudflare.AccessRule{
			Email:          r.Email,
			EmailDomain:    r.EmailDomain,
			IP:             r.IP,
			Everyone:       r.Everyone,
			ServiceToken:   r.ServiceToken,
			GeoCountryCode: r.GeoCountryCode,
		})
	}
	return out
}

func canonicalize(in []cloudflare.AccessRule) []canonicalAccessRule {
	if len(in) == 0 {
		return nil
	}
	out := make([]canonicalAccessRule, 0, len(in))
	for _, r := range in {
		out = append(out, canonicalAccessRule{
			Email:          r.Email,
			EmailDomain:    r.EmailDomain,
			GeoCountryCode: r.GeoCountryCode,
			IP:             r.IP,
			ServiceToken:   r.ServiceToken,
			Everyone:       r.Everyone,
		})
	}
	sort.Slice(out, func(i, j int) bool { return accessRuleSortKey(out[i]) < accessRuleSortKey(out[j]) })
	return out
}

func accessRuleSortKey(r canonicalAccessRule) string {
	switch {
	case r.Email != "":
		return "email\x00" + r.Email
	case r.EmailDomain != "":
		return "emailDomain\x00" + r.EmailDomain
	case r.GeoCountryCode != "":
		return "geoCountryCode\x00" + r.GeoCountryCode
	case r.IP != "":
		return "ip\x00" + r.IP
	case r.ServiceToken != "":
		return "serviceToken\x00" + r.ServiceToken
	case r.Everyone:
		return "everyone\x00true"
	default:
		return ""
	}
}
