package cloudflare_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/andrewreid/cfzt-operator/internal/cloudflare"
)

func TestFakeAccessPolicyCreateGetDelete(t *testing.T) {
	ctx := context.Background()
	fc := cloudflare.NewFake()
	pols := fc.AccessPolicies()

	created, err := pols.Create(ctx, cloudflare.AccessPolicyInput{
		Name:     "allow-staff",
		Decision: "allow",
		Include:  []cloudflare.AccessRule{{EmailDomain: "example.com"}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID == "" {
		t.Fatal("expected non-empty ID from Create")
	}

	got, err := pols.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !reflect.DeepEqual(got, created) {
		t.Fatalf("Get returned different policy:\n got  = %+v\n want = %+v", got, created)
	}

	if err := pols.Delete(ctx, created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := pols.Get(ctx, created.ID); !errors.Is(err, cloudflare.ErrNotFound) {
		t.Fatalf("Get after Delete: want ErrNotFound, got %v", err)
	}
}

func TestFakeAccessPolicyListByName(t *testing.T) {
	ctx := context.Background()
	fc := cloudflare.NewFake()
	pols := fc.AccessPolicies()

	a, err := pols.Create(ctx, cloudflare.AccessPolicyInput{
		Name:     "alpha",
		Decision: "allow",
		Include:  []cloudflare.AccessRule{{Everyone: true}},
	})
	if err != nil {
		t.Fatalf("Create alpha: %v", err)
	}
	b, err := pols.Create(ctx, cloudflare.AccessPolicyInput{
		Name:     "beta",
		Decision: "deny",
		Include:  []cloudflare.AccessRule{{IP: "10.0.0.0/8"}},
	})
	if err != nil {
		t.Fatalf("Create beta: %v", err)
	}

	list, err := pols.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List length = %d, want 2", len(list))
	}
	// Sorted by ID for determinism.
	for i := 1; i < len(list); i++ {
		if list[i-1].ID > list[i].ID {
			t.Fatalf("List not sorted by ID: %+v", list)
		}
	}

	// Caller filters by Name — List does not filter server-side.
	var found *cloudflare.AccessPolicy
	for i := range list {
		if list[i].Name == "beta" {
			found = &list[i]
			break
		}
	}
	if found == nil {
		t.Fatal("filter by Name=beta returned nothing")
	}
	if found.ID != b.ID {
		t.Fatalf("filtered ID = %q, want %q", found.ID, b.ID)
	}
	if found.Decision != "deny" {
		t.Fatalf("filtered Decision = %q, want deny", found.Decision)
	}

	// And 'alpha' is also present.
	var foundA *cloudflare.AccessPolicy
	for i := range list {
		if list[i].Name == "alpha" {
			foundA = &list[i]
			break
		}
	}
	if foundA == nil || foundA.ID != a.ID {
		t.Fatalf("alpha not in list: %+v", list)
	}
}

func TestFakeAccessPolicyUpdateRulesIdempotent(t *testing.T) {
	ctx := context.Background()
	fc := cloudflare.NewFake()
	pols := fc.AccessPolicies()

	in := cloudflare.AccessPolicyInput{
		Name:     "homelab",
		Decision: "allow",
		Include:  []cloudflare.AccessRule{{Email: "andrew@reid.ee"}},
	}
	created, err := pols.Create(ctx, in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	first, err := pols.Update(ctx, created.ID, in)
	if err != nil {
		t.Fatalf("Update#1: %v", err)
	}
	second, err := pols.Update(ctx, created.ID, in)
	if err != nil {
		t.Fatalf("Update#2: %v", err)
	}

	if !reflect.DeepEqual(first, second) {
		t.Fatalf("repeated Update produced drift:\n first  = %+v\n second = %+v", first, second)
	}
	if len(second.Include) != 1 || second.Include[0].Email != "andrew@reid.ee" {
		t.Fatalf("rules duplicated or mutated: %+v", second.Include)
	}
}
