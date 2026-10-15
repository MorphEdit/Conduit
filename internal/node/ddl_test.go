// Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
// Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
// Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)

package node

import (
	"strings"
	"testing"
)

// Shaped like pg_dump 16 --schema-only output.
const dump = `--
-- PostgreSQL database dump
--

\restrict AbC123

SET statement_timeout = 0;
SET client_encoding = 'UTF8';
SELECT pg_catalog.set_config('search_path', '', false);
SET check_function_bodies = false;

CREATE SCHEMA sales;

CREATE FUNCTION public.touch() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  NEW.updated_at := now(); -- a comment; with a semicolon
  RAISE NOTICE 'it''s done; really';
  RETURN NEW;
END $$;

CREATE FUNCTION public.f2() RETURNS text LANGUAGE sql AS $body$ SELECT 'a;b' -- language c, security definer
$body$;

CREATE TABLE public.customers (
    id bigint NOT NULL,
    code text DEFAULT E'x\';y'::text,
    "na;me" text
);

/* block /* nested */ comment; */
COMMENT ON TABLE public.customers IS 'people; and firms';
CREATE SEQUENCE public.customers_id_seq START WITH 1 INCREMENT BY 1;
ALTER TABLE ONLY public.customers ALTER COLUMN id SET DEFAULT nextval('public.customers_id_seq'::regclass);
ALTER TABLE ONLY public.customers ADD CONSTRAINT customers_pkey PRIMARY KEY (id);
CREATE UNIQUE INDEX customers_code ON public.customers USING btree (code);
CREATE MATERIALIZED VIEW public.m AS
 SELECT customers.id
   FROM public.customers
  WITH NO DATA;
CREATE TRIGGER touch BEFORE UPDATE ON public.customers FOR EACH ROW EXECUTE FUNCTION public.touch();

\unrestrict AbC123
`

func TestSchemaStatementsAcceptsADump(t *testing.T) {
	st, err := schemaStatements(dump)
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 15 {
		t.Fatalf("got %d statements:\n%s", len(st), strings.Join(st, "\n---\n"))
	}
	if st[4] != "CREATE SCHEMA IF NOT EXISTS sales" {
		t.Errorf("CREATE SCHEMA should be made idempotent: %q", st[4])
	}
	if !strings.Contains(st[5], "RAISE NOTICE 'it''s done; really'") || !strings.HasSuffix(st[5], "END $$") {
		t.Errorf("function body was split: %q", st[5])
	}
	if !strings.Contains(st[7], `E'x\';y'`) || !strings.Contains(st[7], `"na;me"`) {
		t.Errorf("quoted text was split: %q", st[7])
	}
}

func TestSchemaStatementsRefusesTheRest(t *testing.T) {
	bad := map[string]string{
		"psql shell":           "\\! touch /tmp/pwned\nCREATE TABLE t (id int);",
		"psql after restrict":  "\\restrict k\n\\unrestrict k\n\\! id",
		"copy to program":      "COPY (SELECT 1) TO PROGRAM 'id';",
		"do block":             "DO $$ BEGIN PERFORM 1; END $$;",
		"alter system":         "ALTER SYSTEM SET archive_command = 'id';",
		"extension":            "CREATE EXTENSION plpython3u;",
		"role":                 "CREATE ROLE x SUPERUSER LOGIN;",
		"event trigger":        "CREATE EVENT TRIGGER e ON ddl_command_start EXECUTE FUNCTION f();",
		"select call":          "SELECT public.evil();",
		"set_config then call": "SELECT pg_catalog.set_config('search_path', '', false), public.evil();",
		"security definer":     "CREATE FUNCTION f() RETURNS int LANGUAGE sql SECURITY DEFINER AS $$ SELECT 1 $$;",
		"alter to definer":     "ALTER FUNCTION f() SECURITY DEFINER;",
		"c function":           "CREATE FUNCTION f() RETURNS int LANGUAGE c AS '/tmp/x.so', 'f';",
		"quoted c":             "CREATE FUNCTION f() RETURNS int AS '/tmp/x.so', 'f' LANGUAGE 'c';",
		"c after a body":       "CREATE FUNCTION f() RETURNS int AS $$ x $$ LANGUAGE C;",
		"untrusted language":   "CREATE FUNCTION f() RETURNS int LANGUAGE plpython3u AS $$ return 1 $$;",
		"grant":                "GRANT ALL ON SCHEMA public TO PUBLIC;",
		"create table as":      "CREATE TABLE t AS SELECT public.evil();",
		"create table (x) as":  "CREATE TABLE t (x) AS (SELECT 1);",
		"filled mat view":      "CREATE MATERIALIZED VIEW m AS SELECT public.evil();",
		"unterminated":         "CREATE TABLE t (x text DEFAULT 'oops);",
	}
	for name, script := range bad {
		if st, err := schemaStatements(script); err == nil {
			t.Errorf("%s: accepted %q", name, st)
		}
	}
}
