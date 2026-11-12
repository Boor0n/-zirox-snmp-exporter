package store

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMigrateSingleAdmin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "z.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// An older version kept the only account in settings.
	_ = st.SetSetting(SettingAdmin, "root")
	_ = st.SetSetting(SettingAdminPw, "$2a$10$hash")
	st.Close()

	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	u, err := st.UserByName("root")
	if err != nil || u.Role != RoleAdmin || u.Hash != "$2a$10$hash" {
		t.Fatalf("admin not migrated: %+v %v", u, err)
	}
	if st.GetSetting(SettingAdminPw, "") != "" {
		t.Fatal("old password hash left in settings")
	}
}

func TestSchemaMigrations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "z.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := st.userVersion(); v != SchemaVersion() {
		t.Fatalf("fresh database at schema %d, want %d", v, SchemaVersion())
	}
	if b, _ := filepath.Glob(filepath.Join(dir, "backups", "*")); len(b) != 0 {
		t.Fatalf("fresh database must not be backed up: %v", b)
	}
	// Simulate a database of v0.4 (no user_version) with data in it.
	_ = st.SetSetting("x", "y")
	_, _ = st.db.Exec(`PRAGMA user_version = 0`)
	st.Close()

	saved := migrations
	defer func() { migrations = saved }()
	migrations = append(append([]string{}, saved...), `ALTER TABLE objects ADD COLUMN test_col TEXT NOT NULL DEFAULT ''`)
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := st.userVersion(); v != SchemaVersion() {
		t.Fatalf("schema %d after migration, want %d", v, SchemaVersion())
	}
	if _, err := st.db.Exec(`SELECT test_col FROM objects`); err != nil {
		t.Fatalf("migration not applied: %v", err)
	}
	b, _ := filepath.Glob(filepath.Join(dir, "backups", "*.db"))
	if len(b) != 1 || !strings.Contains(b[0], "before-schema") {
		t.Fatalf("no backup before migration: %v", b)
	}
	st.Close()

	// An older build opening the newer database keeps working.
	migrations = saved
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if st.NewerSchema != SchemaVersion()+1 {
		t.Errorf("NewerSchema = %d", st.NewerSchema)
	}
	if st.GetSetting("x", "") != "y" {
		t.Error("data lost")
	}
}

func TestNoteVersionBacksUp(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "z.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if p, _ := st.NoteVersion("v0.5.0"); p != "" {
		t.Errorf("empty database backed up: %s", p)
	}
	if p, _ := st.NoteVersion("v0.5.0"); p != "" {
		t.Errorf("same version backed up: %s", p)
	}
	p, err := st.NoteVersion("v0.6.0")
	if err != nil || !strings.Contains(p, "before-v0.6.0-from-v0.5.0") {
		t.Fatalf("upgrade backup = %q, %v", p, err)
	}
	for i := 0; i < backupsKept+3; i++ {
		_, _ = st.Backup("t" + strconv.Itoa(i))
		time.Sleep(1100 * time.Millisecond / 100)
	}
	if b, _ := filepath.Glob(filepath.Join(dir, "backups", "*.db")); len(b) > backupsKept {
		t.Errorf("%d backups kept, want at most %d", len(b), backupsKept)
	}
}
