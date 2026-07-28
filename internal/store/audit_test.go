package store_test

import (
	"context"
	"fmt"
	"testing"

	"proxback/internal/store"
)

// Entries come back newest first, and the filters select by action and by actor.
func TestAuditEntriesOrderAndFilters(t *testing.T) {
	st, _ := open(t)
	ctx := context.Background()

	for _, e := range []store.AuditEntry{
		{Actor: "root", ActorID: 1, Action: store.AuditSignIn, ObjectKind: "user", ObjectName: "root"},
		{Actor: "opsy", ActorID: 2, Action: store.AuditRunStart, ObjectKind: "job", ObjectID: "j1", ObjectName: "nightly"},
		{Actor: "opsy", ActorID: 2, Action: store.AuditAccessDenied, Result: store.AuditDenied,
			ObjectKind: "route", ObjectID: "POST /api/targets"},
		{Actor: "root", ActorID: 1, Action: store.AuditRunStart, ObjectKind: "job", ObjectID: "j2", ObjectName: "weekly"},
	} {
		if _, err := st.AppendAudit(ctx, e); err != nil {
			t.Fatalf("append %s: %v", e.Action, err)
		}
	}

	all, err := st.AuditEntries(ctx, store.AuditFilter{})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("entries = %d, want 4", len(all))
	}
	// Newest first, and the timestamp and default result are filled in.
	if all[0].Action != store.AuditRunStart || all[0].ObjectID != "j2" {
		t.Fatalf("newest entry = %+v", all[0])
	}
	if all[0].At.IsZero() || all[0].Result != store.AuditOK {
		t.Fatalf("entry = %+v, want a timestamp and result ok", all[0])
	}
	for i := 1; i < len(all); i++ {
		if all[i].ID >= all[i-1].ID {
			t.Fatalf("entries are not newest first: %d then %d", all[i-1].ID, all[i].ID)
		}
	}

	byAction, err := st.AuditEntries(ctx, store.AuditFilter{Action: store.AuditRunStart})
	if err != nil {
		t.Fatalf("filter by action: %v", err)
	}
	if len(byAction) != 2 {
		t.Fatalf("action filter = %+v, want the two run starts", byAction)
	}
	byActor, err := st.AuditEntries(ctx, store.AuditFilter{Actor: "opsy"})
	if err != nil {
		t.Fatalf("filter by actor: %v", err)
	}
	if len(byActor) != 2 {
		t.Fatalf("actor filter = %+v, want the two entries by opsy", byActor)
	}
	denied, err := st.AuditEntries(ctx, store.AuditFilter{Action: store.AuditAccessDenied, Actor: "opsy"})
	if err != nil {
		t.Fatalf("filter by both: %v", err)
	}
	if len(denied) != 1 || denied[0].Result != store.AuditDenied {
		t.Fatalf("combined filter = %+v", denied)
	}
	limited, err := st.AuditEntries(ctx, store.AuditFilter{Limit: 2})
	if err != nil {
		t.Fatalf("limit: %v", err)
	}
	if len(limited) != 2 || limited[0].ID != all[0].ID {
		t.Fatalf("limit 2 = %+v", limited)
	}
	if _, err := st.AppendAudit(ctx, store.AuditEntry{Actor: "root"}); err == nil {
		t.Fatal("an entry without an action was accepted")
	}
}

// The trail is trimmed to the newest AuditRetention entries on write, so it can
// never grow without bound.
func TestAuditTrimsToRetention(t *testing.T) {
	st, _ := open(t)
	ctx := context.Background()

	original := store.AuditRetention
	store.AuditRetention = 10
	t.Cleanup(func() { store.AuditRetention = original })

	for i := 0; i < 25; i++ {
		if _, err := st.AppendAudit(ctx, store.AuditEntry{
			Action: store.AuditRunStart, Actor: "root",
			ObjectKind: "job", ObjectID: fmt.Sprintf("j%02d", i),
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	n, err := st.CountAuditEntries(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 10 {
		t.Fatalf("entries after 25 writes with retention 10 = %d, want 10", n)
	}
	kept, err := st.AuditEntries(ctx, store.AuditFilter{Limit: store.MaxAuditLimit})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if kept[0].ObjectID != "j24" || kept[len(kept)-1].ObjectID != "j15" {
		t.Fatalf("kept %s..%s, want j24..j15", kept[0].ObjectID, kept[len(kept)-1].ObjectID)
	}
}

// A page is capped, so no client can ask for the whole trail at once, and the
// zero limit means the documented default.
func TestAuditLimitBounds(t *testing.T) {
	st, _ := open(t)
	ctx := context.Background()

	for i := 0; i < store.DefaultAuditLimit+5; i++ {
		if _, err := st.AppendAudit(ctx, store.AuditEntry{
			Action: store.AuditSignIn, Actor: "root",
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	page, err := st.AuditEntries(ctx, store.AuditFilter{})
	if err != nil {
		t.Fatalf("default page: %v", err)
	}
	if len(page) != store.DefaultAuditLimit {
		t.Fatalf("default page = %d entries, want %d", len(page), store.DefaultAuditLimit)
	}
	page, err = st.AuditEntries(ctx, store.AuditFilter{Limit: store.MaxAuditLimit * 10})
	if err != nil {
		t.Fatalf("huge page: %v", err)
	}
	if len(page) != store.DefaultAuditLimit+5 {
		t.Fatalf("capped page = %d entries, want every one of the %d written",
			len(page), store.DefaultAuditLimit+5)
	}
}
