package store

import (
	"fmt"
	"os"
)

// Preset is the tick and role an imported configuration wants for an
// object that the device's next discovery is expected to find.
type Preset struct {
	Kind     string
	Key      string
	Selected bool
	Role     string
}

// PresetKey is the map key of a preset: kind and object key.
func PresetKey(kind, key string) string { return kind + "\x00" + key }

// SetPresets replaces the presets of a device.
func (s *Store) SetPresets(deviceID int64, ps []Preset) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM object_presets WHERE device_id=?`, deviceID); err != nil {
		return err
	}
	for _, p := range ps {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO object_presets(device_id,kind,key,selected,role) VALUES(?,?,?,?,?)`,
			deviceID, p.Kind, p.Key, p.Selected, NormRole(p.Role)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Presets returns a device's presets by PresetKey.
func (s *Store) Presets(deviceID int64) (map[string]Preset, error) {
	rows, err := s.db.Query(`SELECT kind,key,selected,role FROM object_presets WHERE device_id=?`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Preset{}
	for rows.Next() {
		var p Preset
		if err := rows.Scan(&p.Kind, &p.Key, &p.Selected, &p.Role); err != nil {
			return nil, err
		}
		out[PresetKey(p.Kind, p.Key)] = p
	}
	return out, rows.Err()
}

// BackupTo writes a consistent copy of the database to path (which must
// not exist), for downloads.
func (s *Store) BackupTo(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s exists", path)
	}
	_, err := s.db.Exec(`VACUUM INTO ?`, path)
	return err
}
