package kernel

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func auditRows(t *testing.T, k *Kernel) (n int, userID *int64, actor, agent string) {
	t.Helper()
	if err := k.DB.R.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n > 0 {
		if err := k.DB.R.QueryRow(`SELECT user_id, actor, agent FROM audit_log ORDER BY id DESC LIMIT 1`).Scan(&userID, &actor, &agent); err != nil {
			t.Fatal(err)
		}
	}
	return
}

func TestAudit(t *testing.T) {
	k := newKernel(t)
	ctx := context.Background()
	if _, err := k.DB.W.Exec(`INSERT INTO users(id, username, email, created_at, updated_at) VALUES (7, 'robin', 'r@example.com', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	person := &Actor{User: &User{ID: 7}}
	entry := AuditEntry{Action: "settings.test", Category: AuditSettings, Target: "T", Detail: "a\n  b"}
	write := func(a *Actor, e AuditEntry) {
		t.Helper()
		if err := k.Write(ctx, a, func(tx *Tx) error { tx.Audit(e); return nil }); err != nil {
			t.Fatal(err)
		}
	}

	write(SystemActor(), entry)
	write(&Actor{User: &User{ID: 7}, Automation: &AutomationRef{ID: 1}}, entry)
	if n, _, _, _ := auditRows(t, k); n != 0 {
		t.Fatalf("the system actor's and an automation's entries were written: %d rows", n)
	}

	write(person, entry)
	n, uid, _, _ := auditRows(t, k)
	if n != 1 || uid == nil || *uid != 7 {
		t.Fatalf("a person's entry: %d rows, user %v", n, uid)
	}
	var detail string
	if err := k.DB.R.QueryRow(`SELECT detail FROM audit_log`).Scan(&detail); err != nil || detail != "a b" {
		t.Errorf("detail = %q, %v", detail, err)
	}

	forge := entry
	forge.Actor = "octocat"
	write(SystemActor(), forge)
	n, uid, actor, _ := auditRows(t, k)
	if n != 2 || uid != nil || actor != "octocat" {
		t.Errorf("a forge-named entry: %d rows, user %v, actor %q", n, uid, actor)
	}
}

func TestKernelAudit(t *testing.T) {
	k := newKernel(t)
	ctx := context.Background()
	if _, err := k.DB.W.Exec(`INSERT INTO users(id, username, email, created_at, updated_at) VALUES (7, 'robin', 'r@example.com', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	entry := AuditEntry{Action: "token.create", Category: AuditAccess, Target: "T", Detail: "x"}
	k.Audit(ctx, SystemActor(), entry)
	k.Audit(ctx, &Actor{User: &User{ID: 7}, ReadOnly: true}, entry) // a write the actor may not make: logged, not recorded
	if n, _, _, _ := auditRows(t, k); n != 0 {
		t.Fatalf("recorded %d rows for the system actor and a read-only one", n)
	}
	k.Audit(ctx, &Actor{User: &User{ID: 7}}, entry)
	if n, uid, _, _ := auditRows(t, k); n != 1 || uid == nil || *uid != 7 {
		t.Fatalf("a person's entry: %d rows, user %v", n, uid)
	}
}

func TestKernelAuditLogsAFailedWrite(t *testing.T) {
	k := newKernel(t)
	var buf bytes.Buffer
	k.Log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if _, err := k.DB.W.Exec(`INSERT INTO users(id, username, email, created_at, updated_at) VALUES (7, 'robin', 'r@example.com', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	k.Audit(context.Background(), &Actor{User: &User{ID: 7}, ReadOnly: true}, AuditEntry{Action: "token.create", Category: AuditAccess})
	if out := buf.String(); !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "record audit entry") {
		t.Errorf("log = %q, want an error-level \"record audit entry\"", out)
	}
	if n, _, _, _ := auditRows(t, k); n != 0 {
		t.Errorf("%d rows written by a failed write", n)
	}
}

func TestKernelAuditRecordsAForgeNamedEntryWithoutAUser(t *testing.T) {
	k := newKernel(t)
	k.Audit(context.Background(), SystemActor(), AuditEntry{Action: "settings.test", Category: AuditSettings, Actor: "octocat"})
	n, uid, actor, _ := auditRows(t, k)
	if n != 1 || uid != nil || actor != "octocat" {
		t.Errorf("a forge-named entry: %d rows, user %v, actor %q", n, uid, actor)
	}
}
