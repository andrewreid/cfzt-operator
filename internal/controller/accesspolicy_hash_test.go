package controller

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	cfztv1alpha1 "github.com/andrewreid/cfzt-operator/api/v1alpha1"
)

var _ = Describe("CloudflareAccessPolicy rules hash", func() {
	It("TestAccessPolicyRulesHashCanonical", func() {
		first := hashPolicy([]cfztv1alpha1.AccessRule{
			{EmailDomain: testZoneName},
			{Email: testEmail},
		}, nil, nil)
		second := hashPolicy([]cfztv1alpha1.AccessRule{
			{Email: testEmail},
			{EmailDomain: testZoneName},
		}, nil, nil)
		third := hashPolicy(nil, nil, []cfztv1alpha1.AccessRule{
			{Email: testEmail},
			{EmailDomain: testZoneName},
		})

		firstHash, err := accessPolicyRulesHash(first)
		Expect(err).NotTo(HaveOccurred())
		secondHash, err := accessPolicyRulesHash(second)
		Expect(err).NotTo(HaveOccurred())
		thirdHash, err := accessPolicyRulesHash(third)
		Expect(err).NotTo(HaveOccurred())

		Expect(firstHash).To(HavePrefix("sha256:"))
		Expect(strings.TrimPrefix(firstHash, "sha256:")).To(HaveLen(64))
		Expect(firstHash).To(Equal(secondHash))
		Expect(firstHash).NotTo(Equal(thirdHash))
	})
})

func hashPolicy(include, exclude, require []cfztv1alpha1.AccessRule) *cfztv1alpha1.CloudflareAccessPolicy {
	return &cfztv1alpha1.CloudflareAccessPolicy{
		Name: "hash-policy",
		Spec: cfztv1alpha1.CloudflareAccessPolicySpec{
			Decision: testDecisionAllow,
			Rules: cfztv1alpha1.AccessRules{
				Include: include,
				Exclude: exclude,
				Require: require,
			},
		},
	}
}
