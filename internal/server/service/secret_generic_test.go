package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/unidoc/incus-backup/internal/server/service"
)

func TestGenericSecrets(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ids := map[string]string{}
	for _, k := range []string{service.SecretSMTP, service.SecretToken, service.SecretWebhook} {
		id, err := e.svc.CreateSecret(ctx, "notify-"+k, k, []byte(`{"password":"pw-`+k+`"}`))
		if err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		ids[k] = id
	}
	if _, err := e.svc.CreateSecret(ctx, "x", "s3", []byte("x")); !errors.Is(err, service.ErrBadRequest) {
		t.Fatalf("s3 kind through the generic API: %v", err)
	}
	if _, err := e.svc.CreateSecret(ctx, "notify-smtp", service.SecretSMTP, []byte("x")); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("duplicate name: %v", err)
	}
	// the database CHECK itself accepts exactly the four kinds
	for kind, ok := range map[string]bool{"s3": true, "smtp": true, "token": true, "webhook": true, "bogus": false} {
		_, err := e.svc.DB.Pool().Exec(ctx, `INSERT INTO credentials (name, kind, sealed) VALUES ($1,$2,'\x00')`, "chk-"+kind, kind)
		if (err == nil) != ok {
			t.Fatalf("kind %s: %v", kind, err)
		}
	}
	if _, err := e.svc.DB.Pool().Exec(ctx, `DELETE FROM credentials WHERE name LIKE 'chk-%'`); err != nil {
		t.Fatal(err)
	}
	raw, err := e.svc.ReadSecret(ctx, ids[service.SecretToken])
	if err != nil || string(raw) != `{"password":"pw-token"}` {
		t.Fatalf("read: %s %v", raw, err)
	}
	if err := e.svc.RotateSecret(ctx, ids[service.SecretToken], []byte("new")); err != nil {
		t.Fatal(err)
	}
	if raw, _ = e.svc.ReadSecret(ctx, ids[service.SecretToken]); string(raw) != "new" {
		t.Fatalf("after rotate: %s", raw)
	}
	// the ciphertext is bound to the credential id: moving it to another row does not open
	if _, err := e.svc.DB.Pool().Exec(ctx, `UPDATE credentials SET sealed=(SELECT sealed FROM credentials WHERE id=$1) WHERE id=$2`, ids[service.SecretToken], ids[service.SecretSMTP]); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ReadSecret(ctx, ids[service.SecretSMTP]); err == nil {
		t.Fatal("swapped ciphertext opened")
	}
	// listing: generic secrets show no public part, and never their content
	creds, err := e.svc.Credentials(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(creds)
	if len(creds) != 3 || strings.Contains(string(b), "pw-") || strings.Contains(string(b), "new") {
		t.Fatalf("listing: %s", b)
	}
	// the S3 credential commands do not touch generic secrets
	if err := e.svc.DeleteCredential(ctx, "notify-webhook"); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("DeleteCredential removed a generic secret: %v", err)
	}
	if err := e.svc.RotateS3Credential(ctx, "notify-webhook", service.S3Secret{AccessKey: "a", SecretKey: "b"}); err == nil {
		t.Fatal("RotateS3Credential overwrote a generic secret")
	}
	if err := e.svc.DeleteSecret(ctx, ids[service.SecretWebhook]); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ReadSecret(ctx, ids[service.SecretWebhook]); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("deleted secret: %v", err)
	}
}
