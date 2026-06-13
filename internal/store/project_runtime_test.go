package store

import (
	"context"
	"errors"
	"testing"
)

func TestProjectRuntimeRoundTrip(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	const uid, aid, pid = "u_1", "agt_1", "proj_abc"

	// Absent runtime → ErrNotFound.
	if _, err := db.GetProjectRuntime(ctx, uid, aid, pid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing runtime, got %v", err)
	}

	// Insert.
	rec := &ProjectRuntimeRecord{
		UserID:      uid,
		AgentID:     aid,
		ProjectID:   pid,
		TemplateRef: "shipany-tanstack",
		Status:      "running",
		DevPort:     3000,
		HostPort:    49210,
		PreviewURL:  "http://127.0.0.1:49210",
		ContainerID: "deadbeef",
		GitRef:      "abc123",
	}
	if err := db.SaveProjectRuntime(ctx, rec); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := db.GetProjectRuntime(ctx, uid, aid, pid)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.TemplateRef != "shipany-tanstack" || got.DevPort != 3000 || got.HostPort != 49210 {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if got.PreviewURL != "http://127.0.0.1:49210" || got.ContainerID != "deadbeef" {
		t.Fatalf("roundtrip mismatch on url/container: %+v", got)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("expected timestamps stamped, got %+v", got)
	}

	// Upsert: change status + clear host port (sleep), created_at preserved.
	created := got.CreatedAt
	got.Status = "sleeping"
	got.HostPort = 0
	got.PreviewURL = ""
	if err := db.SaveProjectRuntime(ctx, got); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	after, err := db.GetProjectRuntime(ctx, uid, aid, pid)
	if err != nil {
		t.Fatalf("get after upsert: %v", err)
	}
	if after.Status != "sleeping" || after.HostPort != 0 {
		t.Fatalf("upsert did not apply: %+v", after)
	}
	if !after.CreatedAt.Equal(created) {
		t.Fatalf("created_at not preserved across upsert: was %v now %v", created, after.CreatedAt)
	}

	// ListAll sees it regardless of owner scoping.
	all, err := db.ListAllProjectRuntimes(ctx)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 1 || all[0].ProjectID != pid {
		t.Fatalf("expected 1 runtime, got %+v", all)
	}

	// Delete.
	if err := db.DeleteProjectRuntime(ctx, uid, aid, pid); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := db.GetProjectRuntime(ctx, uid, aid, pid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}
