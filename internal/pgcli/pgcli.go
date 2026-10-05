// Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
// Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
// Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)

// Package pgcli prepares connection details for the Postgres command-line
// tools (psql, pg_dump) without putting the password on the command line,
// where any local user could read it from the process list.
package pgcli

import (
	"net/url"
	"os"
	"regexp"
	"strings"
)

var keywordPassword = regexp.MustCompile(`(?i)(^|\s)password\s*=\s*('(?:[^'\\]|\\.)*'|\S+)`)

// Conn returns the connection string to pass as --dbname (with any password
// removed) and the environment to run the tool with (PGPASSWORD set).
func Conn(dsn string) (dbname string, env []string) {
	env = os.Environ()
	if u, err := url.Parse(dsn); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		if u.User != nil {
			if pw, ok := u.User.Password(); ok {
				env = append(env, "PGPASSWORD="+pw)
				u.User = url.User(u.User.Username())
			}
		}
		q := u.Query()
		if pw := q.Get("password"); pw != "" {
			env = append(env, "PGPASSWORD="+pw)
			q.Del("password")
			u.RawQuery = q.Encode()
		}
		return u.String(), env
	}
	// keyword=value form
	if m := keywordPassword.FindStringSubmatch(dsn); m != nil {
		pw := strings.Trim(m[2], "'")
		env = append(env, "PGPASSWORD="+strings.ReplaceAll(pw, `\'`, `'`))
		dsn = strings.TrimSpace(keywordPassword.ReplaceAllString(dsn, "$1"))
	}
	return dsn, env
}
