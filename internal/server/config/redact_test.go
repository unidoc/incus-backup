package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/unidoc/incus-backup/internal/server/webtls"
)

func TestRedactedNeverHoldsSecrets(t *testing.T) {
	c := Defaults()
	c.DatabaseURL = "postgres://ib:Sup3rSecretPw@db.example:5432/ib?sslmode=require&password=AlsoSecret"
	c.Web = webtls.Config{}
	b, _ := json.Marshal(c.Redacted())
	s := string(b)
	if strings.Contains(s, "Sup3rSecretPw") || strings.Contains(s, "AlsoSecret") || !strings.Contains(s, "db.example:5432") || !strings.Contains(s, "state_dir") {
		t.Fatalf("redacted: %s", s)
	}
	if RedactURL("host=db password=x") != RedactedValue {
		t.Fatal("libpq strings must be redacted whole")
	}
}
