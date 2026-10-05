// Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
// Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
// Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)

package pgcli

import (
	"strings"
	"testing"
)

func pgpassword(env []string) string {
	pw := ""
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "PGPASSWORD="); ok {
			pw = v
		}
	}
	return pw
}

func TestConnRemovesPasswordFromURL(t *testing.T) {
	db, env := Conn("postgres://app:s3cr%40t@db:5432/shop?sslmode=disable")
	if strings.Contains(db, "s3cr") {
		t.Fatalf("password still in dbname: %s", db)
	}
	if db != "postgres://app@db:5432/shop?sslmode=disable" {
		t.Fatalf("unexpected dbname %s", db)
	}
	if pgpassword(env) != "s3cr@t" {
		t.Fatalf("PGPASSWORD = %q", pgpassword(env))
	}
}

func TestConnPasswordInQuery(t *testing.T) {
	db, env := Conn("postgresql://db/shop?user=app&password=pw1")
	if strings.Contains(db, "pw1") || pgpassword(env) != "pw1" {
		t.Fatalf("db=%s pw=%s", db, pgpassword(env))
	}
}

func TestConnKeywordForm(t *testing.T) {
	db, env := Conn("host=db user=app password='it''s' dbname=shop")
	if strings.Contains(db, "password") {
		t.Fatalf("password still in dbname: %s", db)
	}
	if pgpassword(env) == "" {
		t.Fatal("PGPASSWORD not set")
	}
}

func TestConnNoPassword(t *testing.T) {
	db, env := Conn("postgres://app@db/shop")
	if db != "postgres://app@db/shop" || pgpassword(env) != "" {
		t.Fatalf("db=%s pw=%q", db, pgpassword(env))
	}
}
