package store

import (
	"path/filepath"
	"testing"
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
