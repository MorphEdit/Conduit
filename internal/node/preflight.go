// Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
// Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
// Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)

package node

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Settings Conduit needs. ALTER SYSTEM makes them permanent, but Postgres
// only picks them up after one restart, which a human should time.
var requiredSettings = [][2]string{
	{"wal_level", "logical"},
	{"track_commit_timestamp", "on"},
}

func missingSettings(ctx context.Context, pool *pgxpool.Pool) ([][2]string, error) {
	var missing [][2]string
	for _, s := range requiredSettings {
		var have string
		if err := pool.QueryRow(ctx, `SELECT current_setting($1)`, s[0]).Scan(&have); err != nil {
			return nil, err
		}
		if have != s[1] {
			missing = append(missing, s)
		}
	}
	return missing, nil
}

func (r *Runtime) ensurePostgres(ctx context.Context, pool *pgxpool.Pool) error {
	altered, allowed := false, r.Cfg.ConfigurePostgres
	for {
		missing, err := missingSettings(ctx, pool)
		r.setErr(err)
		if err == nil && len(missing) == 0 {
			return nil
		}
		names := make([]string, len(missing))
		for i, m := range missing {
			names[i] = m[0] + "=" + m[1]
		}
		if err == nil && !altered && !allowed {
			// Never change a database's settings without a yes: it may be a
			// production database, and the change needs a restart.
			r.setPhase(PhaseNeedsConfig, "Postgres ยังตั้งค่าไม่พร้อม ("+strings.Join(names, ", ")+
				") — กด \"อนุญาตให้ตั้งค่า\" บน dashboard แล้ว restart Postgres หนึ่งครั้ง หรือตั้งค่าเองแล้ว restart")
		}
		if err == nil && !altered && allowed {
			for _, s := range missing {
				// ALTER SYSTEM takes no parameters; names and values are the constants above.
				if _, err = pool.Exec(ctx, "ALTER SYSTEM SET "+s[0]+" = '"+s[1]+"'"); err != nil {
					break
				}
			}
			if err == nil {
				// Reload so pg_settings.pending_restart shows what is waiting.
				_, err = pool.Exec(ctx, `SELECT pg_reload_conf()`)
			}
			r.setErr(err)
			if err == nil {
				altered = true
				r.Log.Warn("changed Postgres settings; restart Postgres once to apply them", "settings", names)
			}
		}
		if altered {
			r.setPhase(PhaseNeedsRestart, "ตั้งค่า Postgres ให้แล้ว ("+strings.Join(names, ", ")+") — restart Postgres หนึ่งครั้งเพื่อให้มีผล")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.allowPG:
			allowed = true
			r.Log.Info("admin allowed changing Postgres settings")
		case <-time.After(5 * time.Second):
		}
	}
}
