// Package store keeps devices, credentials, poll profiles and discovered
// objects in SQLite.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

const schema = `
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS credentials (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	name          TEXT NOT NULL UNIQUE,
	version       TEXT NOT NULL,
	community     TEXT NOT NULL DEFAULT '',
	v3_user       TEXT NOT NULL DEFAULT '',
	v3_level      TEXT NOT NULL DEFAULT '',
	v3_auth_proto TEXT NOT NULL DEFAULT '',
	v3_auth_pass  TEXT NOT NULL DEFAULT '',
	v3_priv_proto TEXT NOT NULL DEFAULT '',
	v3_priv_pass  TEXT NOT NULL DEFAULT '',
	v3_context    TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS poll_profiles (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	name           TEXT NOT NULL UNIQUE,
	interval_sec   INTEGER NOT NULL,
	timeout_sec    INTEGER NOT NULL DEFAULT 5,
	retries        INTEGER NOT NULL DEFAULT 1,
	auto_select_up INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS devices (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	name            TEXT NOT NULL UNIQUE,
	address         TEXT NOT NULL,
	port            INTEGER NOT NULL DEFAULT 161,
	credential_id   INTEGER NOT NULL REFERENCES credentials(id),
	profile_id      INTEGER REFERENCES poll_profiles(id) ON DELETE SET NULL,
	interval_sec    INTEGER NOT NULL DEFAULT 0,
	group_name      TEXT NOT NULL DEFAULT '',
	enabled         INTEGER NOT NULL DEFAULT 1,
	sys_name        TEXT NOT NULL DEFAULT '',
	sys_descr       TEXT NOT NULL DEFAULT '',
	sys_object_id   TEXT NOT NULL DEFAULT '',
	last_discovery  INTEGER NOT NULL DEFAULT 0,
	discovery_error TEXT NOT NULL DEFAULT '',
	created_at      INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS objects (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	device_id   INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
	kind        TEXT NOT NULL,
	key         TEXT NOT NULL,
	idx         TEXT NOT NULL,
	name        TEXT NOT NULL DEFAULT '',
	descr       TEXT NOT NULL DEFAULT '',
	alias       TEXT NOT NULL DEFAULT '',
	if_type     INTEGER NOT NULL DEFAULT 0,
	speed_mbps  INTEGER NOT NULL DEFAULT 0,
	oper_status INTEGER NOT NULL DEFAULT 0,
	factor      REAL NOT NULL DEFAULT 1,
	selected    INTEGER NOT NULL DEFAULT 0,
	state       TEXT NOT NULL DEFAULT 'active',
	UNIQUE(device_id, kind, key)
);
CREATE INDEX IF NOT EXISTS objects_device ON objects(device_id);
`

// Object states.
const (
	StateActive  = "active"
	StateNew     = "new"
	StateMissing = "missing"
)

// Setting keys and defaults.
const (
	SettingInterval = "global_interval_sec"
	SettingTimeout  = "global_timeout_sec"
	SettingRetries  = "global_retries"
	SettingAutoUp   = "global_auto_select_up"
	SettingAdmin    = "admin_user"
	SettingAdminPw  = "admin_hash"
)

// MinIntervalSec is the shortest poll interval allowed anywhere.
const MinIntervalSec = 5

// Credential holds SNMP access data. Secret fields are encrypted by the caller.
type Credential struct {
	ID          int64
	Name        string
	Version     string // "1", "2c" or "3"
	Community   string
	V3User      string
	V3Level     string // noAuthNoPriv, authNoPriv, authPriv
	V3AuthProto string // MD5, SHA, SHA224, SHA256, SHA384, SHA512
	V3AuthPass  string
	V3PrivProto string // DES, AES, AES192, AES256, AES192C, AES256C
	V3PrivPass  string
	V3Context   string
}

// PollProfile groups poll settings that many devices share.
type PollProfile struct {
	ID           int64
	Name         string
	IntervalSec  int
	TimeoutSec   int
	Retries      int
	AutoSelectUp bool
}

// Device is one SNMP agent.
type Device struct {
	ID             int64
	Name           string
	Address        string
	Port           int
	CredentialID   int64
	ProfileID      int64 // 0 = use global settings
	IntervalSec    int   // 0 = inherit from profile/global
	Group          string
	Enabled        bool
	SysName        string
	SysDescr       string
	SysObjectID    string
	LastDiscovery  time.Time
	DiscoveryError string
	CreatedAt      time.Time
}

// Object is a discovered port or health item (CPU, memory, sensor...).
type Object struct {
	ID         int64
	DeviceID   int64
	Kind       string
	Key        string // stable identity across refreshes (ifName, sensor name)
	Idx        string // current SNMP index
	Name       string
	Descr      string
	Alias      string
	IfType     int
	SpeedMbps  int64
	OperStatus int
	Factor     float64
	Selected   bool
	State      string
}

// Settings are the global defaults.
type Settings struct {
	IntervalSec  int
	TimeoutSec   int
	Retries      int
	AutoSelectUp bool
}

// Store wraps the database.
type Store struct {
	db *sql.DB
}

// Open opens (and migrates) the database at path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// ---- settings ----

// GetSetting returns a raw setting value or def.
func (s *Store) GetSetting(key, def string) string {
	var v string
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v); err != nil {
		return def
	}
	return v
}

// SetSetting stores a raw setting value.
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func (s *Store) intSetting(key string, def int) int {
	n, err := strconv.Atoi(s.GetSetting(key, ""))
	if err != nil {
		return def
	}
	return n
}

// Settings returns the global defaults.
func (s *Store) Settings() Settings {
	return Settings{
		IntervalSec:  max(s.intSetting(SettingInterval, 60), MinIntervalSec),
		TimeoutSec:   max(s.intSetting(SettingTimeout, 5), 1),
		Retries:      max(s.intSetting(SettingRetries, 1), 0),
		AutoSelectUp: s.GetSetting(SettingAutoUp, "1") == "1",
	}
}

// SaveSettings stores the global defaults.
func (s *Store) SaveSettings(st Settings) error {
	auto := "0"
	if st.AutoSelectUp {
		auto = "1"
	}
	for k, v := range map[string]string{
		SettingInterval: strconv.Itoa(max(st.IntervalSec, MinIntervalSec)),
		SettingTimeout:  strconv.Itoa(max(st.TimeoutSec, 1)),
		SettingRetries:  strconv.Itoa(max(st.Retries, 0)),
		SettingAutoUp:   auto,
	} {
		if err := s.SetSetting(k, v); err != nil {
			return err
		}
	}
	return nil
}

// ---- credentials ----

const credCols = `id,name,version,community,v3_user,v3_level,v3_auth_proto,v3_auth_pass,v3_priv_proto,v3_priv_pass,v3_context`

func scanCred(sc interface{ Scan(...any) error }) (Credential, error) {
	var c Credential
	err := sc.Scan(&c.ID, &c.Name, &c.Version, &c.Community, &c.V3User, &c.V3Level, &c.V3AuthProto, &c.V3AuthPass, &c.V3PrivProto, &c.V3PrivPass, &c.V3Context)
	return c, err
}

// Credentials lists all credentials.
func (s *Store) Credentials() ([]Credential, error) {
	rows, err := s.db.Query(`SELECT ` + credCols + ` FROM credentials ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Credential
	for rows.Next() {
		c, err := scanCred(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Credential returns one credential.
func (s *Store) Credential(id int64) (Credential, error) {
	c, err := scanCred(s.db.QueryRow(`SELECT `+credCols+` FROM credentials WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// CredentialByName finds a credential by name.
func (s *Store) CredentialByName(name string) (Credential, error) {
	c, err := scanCred(s.db.QueryRow(`SELECT `+credCols+` FROM credentials WHERE name=?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// SaveCredential inserts (ID==0) or updates a credential.
func (s *Store) SaveCredential(c *Credential) error {
	if c.ID == 0 {
		res, err := s.db.Exec(`INSERT INTO credentials(name,version,community,v3_user,v3_level,v3_auth_proto,v3_auth_pass,v3_priv_proto,v3_priv_pass,v3_context) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			c.Name, c.Version, c.Community, c.V3User, c.V3Level, c.V3AuthProto, c.V3AuthPass, c.V3PrivProto, c.V3PrivPass, c.V3Context)
		if err != nil {
			return err
		}
		c.ID, err = res.LastInsertId()
		return err
	}
	_, err := s.db.Exec(`UPDATE credentials SET name=?,version=?,community=?,v3_user=?,v3_level=?,v3_auth_proto=?,v3_auth_pass=?,v3_priv_proto=?,v3_priv_pass=?,v3_context=? WHERE id=?`,
		c.Name, c.Version, c.Community, c.V3User, c.V3Level, c.V3AuthProto, c.V3AuthPass, c.V3PrivProto, c.V3PrivPass, c.V3Context, c.ID)
	return err
}

// DeleteCredential removes a credential that no device uses.
func (s *Store) DeleteCredential(id int64) error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM devices WHERE credential_id=?`, id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("used by %d devices", n)
	}
	_, err := s.db.Exec(`DELETE FROM credentials WHERE id=?`, id)
	return err
}

// ---- poll profiles ----

// Profiles lists poll profiles.
func (s *Store) Profiles() ([]PollProfile, error) {
	rows, err := s.db.Query(`SELECT id,name,interval_sec,timeout_sec,retries,auto_select_up FROM poll_profiles ORDER BY interval_sec, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PollProfile
	for rows.Next() {
		var p PollProfile
		if err := rows.Scan(&p.ID, &p.Name, &p.IntervalSec, &p.TimeoutSec, &p.Retries, &p.AutoSelectUp); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Profile returns one poll profile.
func (s *Store) Profile(id int64) (PollProfile, error) {
	var p PollProfile
	err := s.db.QueryRow(`SELECT id,name,interval_sec,timeout_sec,retries,auto_select_up FROM poll_profiles WHERE id=?`, id).
		Scan(&p.ID, &p.Name, &p.IntervalSec, &p.TimeoutSec, &p.Retries, &p.AutoSelectUp)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// ProfileByName finds a profile by name.
func (s *Store) ProfileByName(name string) (PollProfile, error) {
	var id int64
	if err := s.db.QueryRow(`SELECT id FROM poll_profiles WHERE name=?`, name).Scan(&id); err != nil {
		return PollProfile{}, ErrNotFound
	}
	return s.Profile(id)
}

// SaveProfile inserts or updates a poll profile.
func (s *Store) SaveProfile(p *PollProfile) error {
	p.IntervalSec = max(p.IntervalSec, MinIntervalSec)
	p.TimeoutSec = max(p.TimeoutSec, 1)
	p.Retries = max(p.Retries, 0)
	if p.ID == 0 {
		res, err := s.db.Exec(`INSERT INTO poll_profiles(name,interval_sec,timeout_sec,retries,auto_select_up) VALUES(?,?,?,?,?)`,
			p.Name, p.IntervalSec, p.TimeoutSec, p.Retries, p.AutoSelectUp)
		if err != nil {
			return err
		}
		p.ID, err = res.LastInsertId()
		return err
	}
	_, err := s.db.Exec(`UPDATE poll_profiles SET name=?,interval_sec=?,timeout_sec=?,retries=?,auto_select_up=? WHERE id=?`,
		p.Name, p.IntervalSec, p.TimeoutSec, p.Retries, p.AutoSelectUp, p.ID)
	return err
}

// DeleteProfile removes a profile; its devices fall back to global settings.
func (s *Store) DeleteProfile(id int64) error {
	_, err := s.db.Exec(`DELETE FROM poll_profiles WHERE id=?`, id)
	return err
}

// EnsureDefaultProfiles creates the starter profiles on an empty database.
func (s *Store) EnsureDefaultProfiles() error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM poll_profiles`).Scan(&n); err != nil || n > 0 {
		return err
	}
	for _, p := range []PollProfile{
		{Name: "Fast (6 s)", IntervalSec: 6, TimeoutSec: 3, Retries: 0, AutoSelectUp: true},
		{Name: "Normal (60 s)", IntervalSec: 60, TimeoutSec: 5, Retries: 1, AutoSelectUp: true},
		{Name: "Slow (300 s)", IntervalSec: 300, TimeoutSec: 10, Retries: 2, AutoSelectUp: true},
	} {
		if err := s.SaveProfile(&p); err != nil {
			return err
		}
	}
	return nil
}

// ---- devices ----

const devCols = `id,name,address,port,credential_id,COALESCE(profile_id,0),interval_sec,group_name,enabled,sys_name,sys_descr,sys_object_id,last_discovery,discovery_error,created_at`

func scanDev(sc interface{ Scan(...any) error }) (Device, error) {
	var d Device
	var last, created int64
	err := sc.Scan(&d.ID, &d.Name, &d.Address, &d.Port, &d.CredentialID, &d.ProfileID, &d.IntervalSec, &d.Group, &d.Enabled,
		&d.SysName, &d.SysDescr, &d.SysObjectID, &last, &d.DiscoveryError, &created)
	if last > 0 {
		d.LastDiscovery = time.Unix(last, 0)
	}
	d.CreatedAt = time.Unix(created, 0)
	return d, err
}

// Devices lists all devices.
func (s *Store) Devices() ([]Device, error) {
	rows, err := s.db.Query(`SELECT ` + devCols + ` FROM devices ORDER BY group_name, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		d, err := scanDev(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Device returns one device.
func (s *Store) Device(id int64) (Device, error) {
	d, err := scanDev(s.db.QueryRow(`SELECT `+devCols+` FROM devices WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	return d, err
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// SaveDevice inserts or updates the editable fields of a device.
func (s *Store) SaveDevice(d *Device) error {
	if d.Port == 0 {
		d.Port = 161
	}
	if d.IntervalSec > 0 {
		d.IntervalSec = max(d.IntervalSec, MinIntervalSec)
	}
	if d.ID == 0 {
		d.CreatedAt = time.Now()
		res, err := s.db.Exec(`INSERT INTO devices(name,address,port,credential_id,profile_id,interval_sec,group_name,enabled,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
			d.Name, d.Address, d.Port, d.CredentialID, nullID(d.ProfileID), d.IntervalSec, d.Group, d.Enabled, d.CreatedAt.Unix())
		if err != nil {
			return err
		}
		d.ID, err = res.LastInsertId()
		return err
	}
	_, err := s.db.Exec(`UPDATE devices SET name=?,address=?,port=?,credential_id=?,profile_id=?,interval_sec=?,group_name=?,enabled=? WHERE id=?`,
		d.Name, d.Address, d.Port, d.CredentialID, nullID(d.ProfileID), d.IntervalSec, d.Group, d.Enabled, d.ID)
	return err
}

// SetDeviceEnabled toggles polling of a device.
func (s *Store) SetDeviceEnabled(id int64, on bool) error {
	_, err := s.db.Exec(`UPDATE devices SET enabled=? WHERE id=?`, on, id)
	return err
}

// DeleteDevice removes a device and its objects.
func (s *Store) DeleteDevice(id int64) error {
	_, err := s.db.Exec(`DELETE FROM devices WHERE id=?`, id)
	return err
}

// SetDiscoveryError records a failed discovery.
func (s *Store) SetDiscoveryError(id int64, msg string) error {
	_, err := s.db.Exec(`UPDATE devices SET discovery_error=? WHERE id=?`, msg, id)
	return err
}

// ---- objects ----

const objCols = `id,device_id,kind,key,idx,name,descr,alias,if_type,speed_mbps,oper_status,factor,selected,state`

// Objects returns all objects of a device ordered for display.
func (s *Store) Objects(deviceID int64) ([]Object, error) {
	rows, err := s.db.Query(`SELECT `+objCols+` FROM objects WHERE device_id=? ORDER BY kind<>'if', kind, CAST(idx AS INTEGER), idx`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Object
	for rows.Next() {
		var o Object
		if err := rows.Scan(&o.ID, &o.DeviceID, &o.Kind, &o.Key, &o.Idx, &o.Name, &o.Descr, &o.Alias, &o.IfType, &o.SpeedMbps, &o.OperStatus, &o.Factor, &o.Selected, &o.State); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ApplyDiscovery replaces a device's objects with the merged result of a
// discovery and updates the device's system info, in one transaction.
func (s *Store) ApplyDiscovery(d Device, objs []Object) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE devices SET sys_name=?,sys_descr=?,sys_object_id=?,last_discovery=?,discovery_error='' WHERE id=?`,
		d.SysName, d.SysDescr, d.SysObjectID, time.Now().Unix(), d.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM objects WHERE device_id=?`, d.ID); err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO objects(device_id,kind,key,idx,name,descr,alias,if_type,speed_mbps,oper_status,factor,selected,state) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, o := range objs {
		if _, err := stmt.Exec(d.ID, o.Kind, o.Key, o.Idx, o.Name, o.Descr, o.Alias, o.IfType, o.SpeedMbps, o.OperStatus, o.Factor, o.Selected, o.State); err != nil {
			return fmt.Errorf("insert %s/%s: %w", o.Kind, o.Key, err)
		}
	}
	return tx.Commit()
}

// SaveSelection sets which objects are polled and marks new ones as seen.
func (s *Store) SaveSelection(deviceID int64, selected map[int64]bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE objects SET selected=0, state=CASE WHEN state='new' THEN 'active' ELSE state END WHERE device_id=?`, deviceID); err != nil {
		return err
	}
	for id, on := range selected {
		if !on {
			continue
		}
		if _, err := tx.Exec(`UPDATE objects SET selected=1 WHERE id=? AND device_id=?`, id, deviceID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ObjectCounts returns, per device, the number of selected and total objects.
func (s *Store) ObjectCounts() (map[int64][2]int, error) {
	rows, err := s.db.Query(`SELECT device_id, SUM(selected AND state<>'missing'), COUNT(*) FROM objects GROUP BY device_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][2]int{}
	for rows.Next() {
		var id int64
		var sel, total int
		if err := rows.Scan(&id, &sel, &total); err != nil {
			return nil, err
		}
		out[id] = [2]int{sel, total}
	}
	return out, rows.Err()
}
