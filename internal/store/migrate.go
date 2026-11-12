package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Schema versions are kept in SQLite's user_version. Version 1 is the
// schema of v0.1-v0.4 (created with CREATE TABLE IF NOT EXISTS). Each entry
// of migrations moves the database one version up and runs in a
// transaction. Append only: never edit or reorder a released migration, and
// keep them additive (new tables or columns with defaults) so an older
// Zirox can still open a newer database after a rollback.
var migrations = []string{
	// 2: ports whose ifIndex moved are refreshed automatically; nothing to
	// store yet, the version marks databases opened by v0.5+.
	``,
	// 3: hardware model, software version and serial number of devices.
	`ALTER TABLE devices ADD COLUMN model TEXT NOT NULL DEFAULT '';
	 ALTER TABLE devices ADD COLUMN sw_version TEXT NOT NULL DEFAULT '';
	 ALTER TABLE devices ADD COLUMN serial TEXT NOT NULL DEFAULT '';`,
	// 4: port roles and rules that tick ports and set roles on discovery.
	`ALTER TABLE objects ADD COLUMN role TEXT NOT NULL DEFAULT '';
	 CREATE TABLE IF NOT EXISTS port_rules (
	   id INTEGER PRIMARY KEY,
	   pos INTEGER NOT NULL DEFAULT 0,
	   field TEXT NOT NULL DEFAULT 'any',
	   pattern TEXT NOT NULL,
	   grp TEXT NOT NULL DEFAULT '',
	   action TEXT NOT NULL DEFAULT '',
	   role TEXT NOT NULL DEFAULT ''
	 );`,
}

// backupsKept is how many automatic database backups stay on disk.
const backupsKept = 5

// SchemaVersion is the schema version this build writes.
func SchemaVersion() int { return 1 + len(migrations) }

func (s *Store) userVersion() (int, error) {
	var v int
	err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v)
	return v, err
}

// migrate brings the schema to SchemaVersion, backing the file up first when
// an existing database is about to change.
func (s *Store) migrate(fresh bool) error {
	v, err := s.userVersion()
	if err != nil {
		return err
	}
	if v == 0 {
		v = 1 // baseline: tables created above
	}
	if v > SchemaVersion() {
		s.NewerSchema = v
		return nil
	}
	if v < SchemaVersion() && !fresh {
		if _, err := s.Backup(fmt.Sprintf("before-schema-v%d", SchemaVersion())); err != nil {
			return fmt.Errorf("backup before migration: %w", err)
		}
	}
	for ; v < SchemaVersion(); v++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if q := strings.TrimSpace(migrations[v-1]); q != "" {
			if _, err := tx.Exec(q); err != nil {
				tx.Rollback()
				return fmt.Errorf("schema v%d: %w", v+1, err)
			}
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, v+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	if fresh {
		_, err = s.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, SchemaVersion()))
	}
	return err
}

// Backup writes a consistent copy of the database next to it and returns
// its path. Only the newest backupsKept automatic copies are kept.
func (s *Store) Backup(reason string) (string, error) {
	if s.path == "" {
		return "", nil
	}
	dir := filepath.Join(filepath.Dir(s.path), "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := fmt.Sprintf("zirox-%s-%s.db", time.Now().Format("20060102-150405"), safeName(reason))
	out := filepath.Join(dir, name)
	if _, err := s.db.Exec(`VACUUM INTO ?`, out); err != nil {
		return "", err
	}
	_ = os.Chmod(out, 0o600)
	prune(dir)
	return out, nil
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' {
			return r
		}
		return '-'
	}, s)
}

func prune(dir string) {
	m, _ := filepath.Glob(filepath.Join(dir, "zirox-*.db"))
	sort.Strings(m) // names start with the timestamp
	for len(m) > backupsKept {
		_ = os.Remove(m[0])
		m = m[1:]
	}
}

// SettingAppVersion is the Zirox version that last opened the database.
const SettingAppVersion = "app_version"

// NoteVersion records the running version and backs the database up when it
// differs from the one that ran before (an upgrade or a rollback). It
// returns the backup path, or "" when nothing changed.
func (s *Store) NoteVersion(version string) (string, error) {
	prev := s.GetSetting(SettingAppVersion, "")
	if prev == version {
		return "", nil
	}
	var path string
	if prev != "" || s.hasDevices() {
		from := prev
		if from == "" {
			from = "old"
		}
		var err error
		if path, err = s.Backup("before-" + version + "-from-" + from); err != nil {
			return "", err
		}
	}
	return path, s.SetSetting(SettingAppVersion, version)
}

func (s *Store) hasDevices() bool {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM devices`).Scan(&n)
	return n > 0
}
