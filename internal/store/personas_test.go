package store

// Personas storage tests over the shared in-memory FakeDynamo: CRUD, the
// share/unshare write-through CATALOG mirror, delete write-through, and
// the single-partition Query access patterns.

import (
	"context"
	"errors"
	"testing"

	"github.com/JeremyProffittOrg/live-ninja/internal/testutil"
)

func personasStore() (*Store, *testutil.FakeDynamo) {
	fake := testutil.NewFakeDynamo()
	return NewWithClient(fake, "live-ninja"), fake
}

func seedPersona(t *testing.T, s *Store, userID, id, name string) *UserPersona {
	t.Helper()
	p := &UserPersona{
		PersonaID:    id,
		Name:         name,
		Description:  "desc " + name,
		Instructions: "Talk like " + name + ".",
		Voice:        "cedar",
	}
	if err := s.CreateUserPersona(context.Background(), userID, p); err != nil {
		t.Fatalf("seed persona %s: %v", id, err)
	}
	return p
}

func TestPersonaCreateGetList(t *testing.T) {
	s, _ := personasStore()
	ctx := context.Background()
	seedPersona(t, s, "u1", "p1", "DJ")
	seedPersona(t, s, "u1", "p2", "Chef")
	seedPersona(t, s, "u2", "p3", "OtherUser")

	got, err := s.GetUserPersona(ctx, "u1", "p1")
	if err != nil || got == nil || got.Name != "DJ" || got.CreatedAt == "" || got.UpdatedAt == "" {
		t.Fatalf("get persona = %+v, err %v", got, err)
	}
	// Cross-user isolation: u2's persona is invisible from u1's partition.
	if p, err := s.GetUserPersona(ctx, "u1", "p3"); err != nil || p != nil {
		t.Errorf("cross-user get = %+v, err %v; want nil,nil", p, err)
	}

	list, err := s.ListUserPersonas(ctx, "u1")
	if err != nil || len(list) != 2 {
		t.Fatalf("list personas = %d entries, err %v; want 2", len(list), err)
	}

	// Duplicate ID: conditional put loses -> ErrAlreadyExists.
	err = s.CreateUserPersona(ctx, "u1", &UserPersona{PersonaID: "p1", Name: "x", Instructions: "y"})
	if !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("duplicate create err = %v, want ErrAlreadyExists", err)
	}

	// Invalid ids are rejected before any I/O.
	if err := s.CreateUserPersona(ctx, "u1", &UserPersona{PersonaID: "a:b", Name: "x", Instructions: "y"}); err == nil {
		t.Errorf("id with ':' accepted")
	}
	if err := s.CreateUserPersona(ctx, "u1", &UserPersona{PersonaID: "a#b", Name: "x", Instructions: "y"}); err == nil {
		t.Errorf("id with '#' accepted")
	}
}

func TestPersonaShareWriteThrough(t *testing.T) {
	s, fake := personasStore()
	ctx := context.Background()
	seedPersona(t, s, "u1", "p1", "DJ")

	// Share: user item flips + CATALOG mirror appears with attribution.
	p, err := s.SetUserPersonaShared(ctx, "u1", "Jeremy", "p1", true)
	if err != nil || !p.Shared {
		t.Fatalf("share = %+v, err %v", p, err)
	}
	cp, err := s.GetCatalogPersona(ctx, "p1")
	if err != nil || cp == nil || !cp.Shared || cp.OwnerID != "u1" || cp.OwnerName != "Jeremy" ||
		cp.Instructions != "Talk like DJ." {
		t.Fatalf("catalog mirror = %+v, err %v", cp, err)
	}
	shared, err := s.ListSharedPersonas(ctx)
	if err != nil || len(shared) != 1 || shared[0].PersonaID != "p1" {
		t.Fatalf("shared list = %+v, err %v", shared, err)
	}

	// Edit while shared refreshes the mirror write-through.
	p.Name = "DJ Prime"
	p.Instructions = "Talk like DJ Prime."
	if err := s.UpdateUserPersona(ctx, "u1", "Jeremy", p); err != nil {
		t.Fatalf("update while shared: %v", err)
	}
	cp, _ = s.GetCatalogPersona(ctx, "p1")
	if cp == nil || cp.Name != "DJ Prime" || cp.Instructions != "Talk like DJ Prime." {
		t.Fatalf("mirror not refreshed on edit: %+v", cp)
	}

	// Unshare: mirror removed, user item kept.
	if _, err := s.SetUserPersonaShared(ctx, "u1", "Jeremy", "p1", false); err != nil {
		t.Fatalf("unshare: %v", err)
	}
	if cp, _ := s.GetCatalogPersona(ctx, "p1"); cp != nil {
		t.Errorf("mirror survived unshare: %+v", cp)
	}
	if own, _ := s.GetUserPersona(ctx, "u1", "p1"); own == nil || own.Shared {
		t.Errorf("user item wrong after unshare: %+v", own)
	}
	if fake.RawItem("CATALOG", "PERSONA#p1") != nil {
		t.Errorf("raw mirror item still present after unshare")
	}

	// Sharing an absent persona: ErrNotFound.
	if _, err := s.SetUserPersonaShared(ctx, "u1", "Jeremy", "nope", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("share absent err = %v, want ErrNotFound", err)
	}
}

func TestPersonaDeleteWriteThrough(t *testing.T) {
	s, fake := personasStore()
	ctx := context.Background()
	seedPersona(t, s, "u1", "p1", "DJ")
	if _, err := s.SetUserPersonaShared(ctx, "u1", "Jeremy", "p1", true); err != nil {
		t.Fatalf("share: %v", err)
	}

	if err := s.DeleteUserPersona(ctx, "u1", "p1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if own, _ := s.GetUserPersona(ctx, "u1", "p1"); own != nil {
		t.Errorf("user item survived delete")
	}
	if fake.RawItem("CATALOG", "PERSONA#p1") != nil {
		t.Errorf("catalog mirror survived delete (write-through broken)")
	}
	// Deleting again is an idempotent no-op.
	if err := s.DeleteUserPersona(ctx, "u1", "p1"); err != nil {
		t.Errorf("re-delete err = %v, want nil", err)
	}
}

func TestPersonaUpdateAbsent(t *testing.T) {
	s, _ := personasStore()
	err := s.UpdateUserPersona(context.Background(), "u1", "Jeremy", &UserPersona{
		PersonaID: "ghost", Name: "x", Instructions: "y",
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("update absent err = %v, want ErrNotFound", err)
	}
}
