package controller

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cfztv1alpha1 "github.com/andrewreid/cfzt-operator/api/v1alpha1"
)

var _ = Describe("CloudflareAccessPolicy rules hash", func() {
	It("TestAccessPolicyRulesHashCanonical", func() {
		first := hashPolicy([]cfztv1alpha1.AccessRule{
			{EmailDomain: "example.com"},
			{Email: "alice@example.com"},
		}, nil, nil)
		second := hashPolicy([]cfztv1alpha1.AccessRule{
			{Email: "alice@example.com"},
			{EmailDomain: "example.com"},
		}, nil, nil)
		third := hashPolicy(nil, nil, []cfztv1alpha1.AccessRule{
			{Email: "alice@example.com"},
			{EmailDomain: "example.com"},
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
		ObjectMeta: metav1.ObjectMeta{Name: "hash-policy"},
		Spec: cfztv1alpha1.CloudflareAccessPolicySpec{
			Decision: "allow",
			Rules: cfztv1alpha1.AccessRules{
				Include: include,
				Exclude: exclude,
				Require: require,
			},
		},
	}
}
