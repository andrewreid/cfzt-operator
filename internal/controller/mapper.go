package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func enqueueNamed[T client.Object](extract func(T) []types.NamespacedName) handler.MapFunc {
	return func(_ context.Context, obj client.Object) []reconcile.Request {
		typed, ok := obj.(T)
		if !ok {
			return nil
		}
		names := extract(typed)
		if len(names) == 0 {
			return nil
		}
		requests := make([]reconcile.Request, 0, len(names))
		for _, name := range names {
			requests = append(requests, reconcile.Request{NamespacedName: name})
		}
		return requests
	}
}
