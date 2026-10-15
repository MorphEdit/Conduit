// Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
// Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
// Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)

package node

import (
	"fmt"
	"regexp"
	"strings"
)

// A new, empty site copies the table structure from another site
// (pg_dump --schema-only). That script comes over the network and runs as
// Conduit's superuser, so it is not handed to psql, whose meta-commands
// (\! and friends) would run shell commands on this machine. Instead it is
// split into statements and each must be one a schema-only dump of tables
// contains; anything else (COPY ... PROGRAM, DO, ALTER SYSTEM, CREATE
// EXTENSION, roles, event triggers, SECURITY DEFINER or C functions...) stops
// the copy.

var (
	allowedDDL = regexp.MustCompile(`^(?:SET\s|COMMENT\s+ON\s|` +
		`ALTER\s+(?:TABLE|SEQUENCE|INDEX|TYPE|DOMAIN|FUNCTION|PROCEDURE|VIEW|MATERIALIZED\s+VIEW)\s|` +
		`CREATE\s+(?:OR\s+REPLACE\s+)?(?:` +
		`(?:UNLOGGED\s+)?TABLE|SEQUENCE|(?:UNIQUE\s+)?INDEX|VIEW|MATERIALIZED\s+VIEW|TYPE|DOMAIN|SCHEMA|` +
		`FUNCTION|PROCEDURE|AGGREGATE|(?:CONSTRAINT\s+)?TRIGGER|STATISTICS|POLICY|COLLATION)\s)`)
	// pg_dump's own calls, and nothing after them.
	allowedSelect = regexp.MustCompile(`^SELECT\s+pg_catalog\.(?:set_config|setval)\((?:'(?:[^']|'')*'|[^'();])*\)$`)
	createSchema  = regexp.MustCompile(`^CREATE\s+SCHEMA\s+("(?:[^"]|"")+"|[A-Za-z_][A-Za-z0-9_$]*)$`)
	securityDef   = regexp.MustCompile(`(?i)\bSECURITY\s+DEFINER\b`)
	language      = regexp.MustCompile(`(?i)\bLANGUAGE\s+'?([A-Za-z0-9_]+)'?`)
	// CREATE TABLE ... AS SELECT and a materialized view filled at once run a
	// query (and any function in it) while copying; pg_dump never writes them.
	createAsQuery = regexp.MustCompile(`(?s)^CREATE\s+(?:UNLOGGED\s+)?TABLE\b.*\bAS\s*\(?\s*(?:SELECT|VALUES|TABLE|EXECUTE|WITH)\b`)
	matView       = regexp.MustCompile(`^CREATE\s+(?:OR\s+REPLACE\s+)?MATERIALIZED\s+VIEW\b`)
	noData        = regexp.MustCompile(`\bWITH\s+NO\s+DATA\s*$`)
	// psql 16.10+/17.6+ wraps dumps in these; they mean nothing to the server.
	restrictLine = regexp.MustCompile(`^\\(?:un)?restrict\s+[A-Za-z0-9]+\s*$`)
)

// schemaStatements checks a schema-only dump and returns its statements,
// ready to run one by one.
func schemaStatements(script string) ([]string, error) {
	stmts, err := splitSQL(script)
	if err != nil {
		return nil, err
	}
	out := stmts[:0]
	for _, st := range stmts {
		if restrictLine.MatchString(st) {
			continue
		}
		upper := strings.ToUpper(st)
		head := withoutBodies(st) // function bodies may mention anything
		switch {
		case strings.HasPrefix(st, `\`):
			return nil, fmt.Errorf("schema from the other site contains a psql command: %.60q", st)
		case securityDef.MatchString(head):
			return nil, fmt.Errorf("schema from the other site defines a SECURITY DEFINER function: %.60q", st)
		case allowedSelect.MatchString(st):
		case createAsQuery.MatchString(strings.ToUpper(head)) || matView.MatchString(upper) && !noData.MatchString(upper):
			return nil, fmt.Errorf("schema from the other site fills a table from a query: %.60q", st)
		case allowedDDL.MatchString(upper):
			if strings.Contains(upper, "FUNCTION") || strings.Contains(upper, "PROCEDURE") || strings.Contains(upper, "AGGREGATE") {
				for _, m := range language.FindAllStringSubmatch(head, -1) {
					if l := strings.ToLower(m[1]); l != "sql" && l != "plpgsql" {
						return nil, fmt.Errorf("schema from the other site uses LANGUAGE %s (only sql and plpgsql are copied): %.60q", l, st)
					}
				}
			}
			if m := createSchema.FindStringSubmatch(st); m != nil {
				st = "CREATE SCHEMA IF NOT EXISTS " + m[1] // "public" exists already
			}
		default:
			return nil, fmt.Errorf("schema from the other site contains a statement that is not table structure: %.60q", st)
		}
		out = append(out, st)
	}
	return out, nil
}

// splitSQL splits a script into statements at semicolons outside quotes,
// dollar quotes and comments. Comments are dropped; a line that starts a
// statement with a backslash (a psql meta-command) is its own statement.
func splitSQL(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	flush := func() {
		if st := strings.TrimSpace(cur.String()); st != "" {
			out = append(out, st)
		}
		cur.Reset()
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\\' && strings.TrimSpace(cur.String()) == "":
			end := strings.IndexByte(s[i:], '\n')
			if end < 0 {
				end = len(s) - i
			}
			cur.WriteString(s[i : i+end])
			flush()
			i += end
		case c == '-' && strings.HasPrefix(s[i:], "--"):
			end := strings.IndexByte(s[i:], '\n')
			if end < 0 {
				end = len(s) - i
			}
			cur.WriteByte(' ')
			i += end
		case c == '/' && strings.HasPrefix(s[i:], "/*"):
			depth, j := 0, i
			for j < len(s) {
				if strings.HasPrefix(s[j:], "/*") {
					depth, j = depth+1, j+2
				} else if strings.HasPrefix(s[j:], "*/") {
					depth, j = depth-1, j+2
					if depth == 0 {
						break
					}
				} else {
					j++
				}
			}
			if depth != 0 {
				return nil, fmt.Errorf("unterminated comment")
			}
			cur.WriteByte(' ')
			i = j
		case c == '\'' || c == '"':
			// E'...' strings take backslash escapes.
			escapes := c == '\'' && i > 0 && (s[i-1] == 'E' || s[i-1] == 'e') && (i < 2 || !isIdent(s[i-2]))
			j := i + 1
			for ; j < len(s); j++ {
				if escapes && s[j] == '\\' {
					j++
					continue
				}
				if s[j] == c {
					if j+1 < len(s) && s[j+1] == c {
						j++
						continue
					}
					break
				}
			}
			if j >= len(s) {
				return nil, fmt.Errorf("unterminated quoted text")
			}
			cur.WriteString(s[i : j+1])
			i = j + 1
		case c == '$' && (i == 0 || !isIdent(s[i-1])):
			tag := dollarTag(s[i:])
			if tag == "" {
				cur.WriteByte(c)
				i++
				break
			}
			end := strings.Index(s[i+len(tag):], tag)
			if end < 0 {
				return nil, fmt.Errorf("unterminated %s quote", tag)
			}
			n := len(tag) + end + len(tag)
			cur.WriteString(s[i : i+n])
			i += n
		case c == ';':
			flush()
			i++
		default:
			cur.WriteByte(c)
			i++
		}
	}
	flush()
	return out, nil
}

// withoutBodies drops dollar-quoted text (function bodies) from a statement.
func withoutBodies(st string) string {
	var b strings.Builder
	for i := 0; i < len(st); {
		if st[i] == '$' && (i == 0 || !isIdent(st[i-1])) {
			if tag := dollarTag(st[i:]); tag != "" {
				if end := strings.Index(st[i+len(tag):], tag); end >= 0 {
					b.WriteString(" $$ ")
					i += len(tag) + end + len(tag)
					continue
				}
			}
		}
		b.WriteByte(st[i])
		i++
	}
	return b.String()
}

func isIdent(c byte) bool {
	return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

// dollarTag returns the $tag$ that s starts with, or "".
func dollarTag(s string) string {
	for j := 1; j < len(s); j++ {
		c := s[j]
		if c == '$' {
			return s[:j+1]
		}
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80 || j > 1 && c >= '0' && c <= '9') {
			return ""
		}
	}
	return ""
}
