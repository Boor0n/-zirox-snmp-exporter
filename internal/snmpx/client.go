// Package snmpx talks SNMP to devices: discovery walks and lightweight polls.
package snmpx

import (
	"fmt"
	"strings"
	"time"

	"github.com/gosnmp/gosnmp"

	"github.com/Boor0n/zirox-snmp-exporter/internal/store"
)

// maxOidsPerGet caps the varbinds sent in one GET request.
const maxOidsPerGet = 30

// Session is the subset of SNMP operations the app needs. Tests use a fake.
type Session interface {
	Walk(root string) ([]gosnmp.SnmpPDU, error)
	Get(oids []string) ([]gosnmp.SnmpPDU, error)
	Version() string
	Close()
}

// Target describes how to reach one device. Credential secrets are plain text.
type Target struct {
	Address string
	Port    int
	Cred    store.Credential
	Timeout time.Duration
	Retries int
}

// Dialer opens sessions. The default dials real devices with gosnmp.
type Dialer func(t Target) (Session, error)

// Dial connects to a device over UDP.
func Dial(t Target) (Session, error) {
	g := &gosnmp.GoSNMP{
		Target:         t.Address,
		Port:           uint16(t.Port),
		Transport:      "udp",
		Timeout:        t.Timeout,
		Retries:        t.Retries,
		MaxOids:        maxOidsPerGet,
		MaxRepetitions: 25,
	}
	if g.Port == 0 {
		g.Port = 161
	}
	switch t.Cred.Version {
	case "1":
		g.Version = gosnmp.Version1
		g.Community = t.Cred.Community
	case "2c", "":
		g.Version = gosnmp.Version2c
		g.Community = t.Cred.Community
	case "3":
		g.Version = gosnmp.Version3
		g.SecurityModel = gosnmp.UserSecurityModel
		g.ContextName = t.Cred.V3Context
		usm := &gosnmp.UsmSecurityParameters{UserName: t.Cred.V3User}
		switch t.Cred.V3Level {
		case "authPriv":
			g.MsgFlags = gosnmp.AuthPriv
		case "authNoPriv":
			g.MsgFlags = gosnmp.AuthNoPriv
		default:
			g.MsgFlags = gosnmp.NoAuthNoPriv
		}
		if g.MsgFlags != gosnmp.NoAuthNoPriv {
			ap, err := authProto(t.Cred.V3AuthProto)
			if err != nil {
				return nil, err
			}
			usm.AuthenticationProtocol = ap
			usm.AuthenticationPassphrase = t.Cred.V3AuthPass
		}
		if g.MsgFlags == gosnmp.AuthPriv {
			pp, err := privProto(t.Cred.V3PrivProto)
			if err != nil {
				return nil, err
			}
			usm.PrivacyProtocol = pp
			usm.PrivacyPassphrase = t.Cred.V3PrivPass
		}
		g.SecurityParameters = usm
	default:
		return nil, fmt.Errorf("unknown SNMP version %q", t.Cred.Version)
	}
	if err := g.Connect(); err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return &session{g: g, version: t.Cred.Version}, nil
}

func authProto(s string) (gosnmp.SnmpV3AuthProtocol, error) {
	switch strings.ToUpper(s) {
	case "MD5":
		return gosnmp.MD5, nil
	case "SHA", "SHA1", "":
		return gosnmp.SHA, nil
	case "SHA224":
		return gosnmp.SHA224, nil
	case "SHA256":
		return gosnmp.SHA256, nil
	case "SHA384":
		return gosnmp.SHA384, nil
	case "SHA512":
		return gosnmp.SHA512, nil
	}
	return 0, fmt.Errorf("unknown auth protocol %q", s)
}

func privProto(s string) (gosnmp.SnmpV3PrivProtocol, error) {
	switch strings.ToUpper(s) {
	case "DES":
		return gosnmp.DES, nil
	case "AES", "AES128", "":
		return gosnmp.AES, nil
	case "AES192":
		return gosnmp.AES192, nil
	case "AES256":
		return gosnmp.AES256, nil
	case "AES192C":
		return gosnmp.AES192C, nil
	case "AES256C":
		return gosnmp.AES256C, nil
	}
	return 0, fmt.Errorf("unknown privacy protocol %q", s)
}

type session struct {
	g       *gosnmp.GoSNMP
	version string
}

func (s *session) Version() string { return s.version }

func (s *session) Close() {
	if s.g.Conn != nil {
		s.g.Conn.Close()
	}
}

func (s *session) Walk(root string) ([]gosnmp.SnmpPDU, error) {
	if s.g.Version == gosnmp.Version1 {
		return s.g.WalkAll(root)
	}
	return s.g.BulkWalkAll(root)
}

// Get fetches oids in chunks. Missing instances are dropped from the result.
func (s *session) Get(oids []string) ([]gosnmp.SnmpPDU, error) {
	var out []gosnmp.SnmpPDU
	for start := 0; start < len(oids); start += maxOidsPerGet {
		chunk := oids[start:min(start+maxOidsPerGet, len(oids))]
		pkt, err := s.g.Get(chunk)
		if err != nil {
			return out, err
		}
		if pkt.Error != gosnmp.NoError {
			// SNMPv1 fails the whole request when one OID is unknown;
			// fall back to one OID per request for this chunk.
			if s.g.Version != gosnmp.Version1 || len(chunk) == 1 {
				continue
			}
			for _, oid := range chunk {
				p, err := s.g.Get([]string{oid})
				if err != nil {
					return out, err
				}
				if p.Error == gosnmp.NoError {
					out = append(out, valid(p.Variables)...)
				}
			}
			continue
		}
		out = append(out, valid(pkt.Variables)...)
	}
	return out, nil
}

func valid(pdus []gosnmp.SnmpPDU) []gosnmp.SnmpPDU {
	out := pdus[:0:0]
	for _, p := range pdus {
		switch p.Type {
		case gosnmp.NoSuchObject, gosnmp.NoSuchInstance, gosnmp.EndOfMibView, gosnmp.Null:
			continue
		}
		out = append(out, p)
	}
	return out
}
