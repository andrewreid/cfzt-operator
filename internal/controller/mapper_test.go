package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cfztv1alpha1 "github.com/andrewreid/cfzt-operator/api/v1alpha1"
)

var _ = Describe("Mapper helper", func() {
	It("TestEnqueueNamedExtracts", func() {
		mapFunc := enqueueNamed(func(tunnel *cfztv1alpha1.CloudflareTunnel) []types.NamespacedName {
			return []types.NamespacedName{
				{Name: tunnel.Name},
				{Namespace: "shadow", Name: tunnel.Name + "-shadow"},
			}
		})

		requests := mapFunc(context.Background(), &cfztv1alpha1.CloudflareTunnel{
			ObjectMeta: metav1.ObjectMeta{Name: "mapped-tunnel"},
		})

		Expect(requests).To(ConsistOf(
			reconcile.Request{NamespacedName: types.NamespacedName{Name: "mapped-tunnel"}},
			reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "shadow", Name: "mapped-tunnel-shadow"}},
		))
	})
})
