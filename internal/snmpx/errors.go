package snmpx

import (
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/gosnmp/gosnmp"
)

// Problem classes of a failed SNMP request. They tell the user what to
// check instead of the raw library message.
const (
	ProblemTimeout    = "timeout"
	ProblemNoRoute    = "no_route"
	ProblemRefused    = "refused"
	ProblemDNS        = "dns"
	ProblemAuthz      = "authorization"
	ProblemV3User     = "v3_user"
	ProblemV3Auth     = "v3_auth"
	ProblemV3Priv     = "v3_priv"
	ProblemV3Time     = "v3_time"
	ProblemNoData     = "no_data"
	ProblemTooBig     = "too_big"
	ProblemGenErr     = "gen_err"
	ProblemConfig     = "config"
	ProblemUnknown    = "unknown"
	ProblemMismatch   = "index_shift"
	ProblemPartial    = "partial"
	ProblemCredential = "credential"
)

// StatusError is an SNMP response that carried an error status instead of
// values, such as authorizationError.
type StatusError struct {
	Status gosnmp.SNMPError
}

func (e *StatusError) Error() string {
	return "device answered with " + statusName(e.Status)
}

func statusName(s gosnmp.SNMPError) string {
	n := s.String()
	if n == "" || strings.HasPrefix(n, "SNMPError(") {
		return fmt.Sprintf("error status %d", int(s))
	}
	return n
}

// ErrNoData means the device answered but returned no system values.
var ErrNoData = errors.New("device answered but returned no sysDescr/sysName")

// Explained is an SNMP failure in plain words.
type Explained struct {
	Problem string // one of the Problem* constants
	Summary string // one line, what happened
	Hint    string // what to check
	Raw     string // the original error text
}

// Error is the summary with the hint, for places that keep one string.
func (e Explained) Error() string {
	s := e.Summary
	if e.Hint != "" {
		s += ". " + e.Hint
	}
	if e.Raw != "" && !strings.Contains(s, e.Raw) {
		s += " [" + e.Raw + "]"
	}
	return s
}

// Explain turns an SNMP error into a problem class with a hint. source is
// the address Zirox sends from, when known, so the hint can name it.
func Explain(err error, version, source string) Explained {
	if err == nil {
		return Explained{}
	}
	raw := err.Error()
	low := strings.ToLower(raw)
	from := "the Zirox server"
	if source != "" {
		from = source
	}
	var se *StatusError
	switch {
	case errors.As(err, &se) && (se.Status == gosnmp.AuthorizationError || se.Status == gosnmp.NoAccess):
		return Explained{ProblemAuthz, "The device refused access (" + statusName(se.Status) + ")",
			"The community or v3 user is accepted but its view does not allow these OIDs. On Cisco check 'snmp-server community <name> view <view> RO <acl>'. A community containing '@' is read by Cisco as community@context", raw}
	case errors.As(err, &se) && se.Status == gosnmp.TooBig:
		return Explained{ProblemTooBig, "The answer was too big for one packet", "Lower the number of OIDs per request or check the MTU on the path", raw}
	case errors.As(err, &se):
		return Explained{ProblemGenErr, "The device answered with " + statusName(se.Status), "The agent could not read some values; usually a device-side bug. Try Refresh", raw}
	case errors.Is(err, gosnmp.ErrUnknownUsername):
		return Explained{ProblemV3User, "Unknown SNMPv3 user", "The user name does not exist on the device ('snmp-server user <name> <group> v3 ...')", raw}
	case errors.Is(err, gosnmp.ErrWrongDigest) || strings.Contains(low, "not authentic"):
		return Explained{ProblemV3Auth, "SNMPv3 authentication failed", "Wrong auth password or protocol (MD5/SHA/SHA256...)", raw}
	case errors.Is(err, gosnmp.ErrDecryption) || strings.Contains(low, "decrypt"):
		return Explained{ProblemV3Priv, "SNMPv3 decryption failed", "Wrong privacy password or protocol (DES/AES/AES256...)", raw}
	case errors.Is(err, gosnmp.ErrUnknownSecurityLevel):
		return Explained{ProblemV3Auth, "SNMPv3 security level not supported for this user", "Check the level (noAuthNoPriv/authNoPriv/authPriv) matches the user's group on the device", raw}
	case errors.Is(err, gosnmp.ErrNotInTimeWindow) || errors.Is(err, gosnmp.ErrUnknownEngineID):
		return Explained{ProblemV3Time, "SNMPv3 engine time or ID mismatch", "Usually clears on the next request; if it persists the device clock or engine ID changed", raw}
	case errors.Is(err, ErrNoData):
		return Explained{ProblemNoData, "The device answered but returned no system values", "The view of this community or user hides the 'system' group (1.3.6.1.2.1.1)", raw}
	case strings.Contains(low, "no route to host") || strings.Contains(low, "network is unreachable") || strings.Contains(low, "host is unreachable"):
		return Explained{ProblemNoRoute, "No network route from Zirox to the device", "Check routing from " + from + ". If Zirox runs in Docker and the host can reach the device, reinstall with get.sh --host-network", raw}
	case strings.Contains(low, "connection refused"):
		return Explained{ProblemRefused, "The device rejected UDP port 161", "The SNMP agent is not running or listens on another port ('snmp-server community ...' missing)", raw}
	case strings.Contains(low, "no such host") || strings.Contains(low, "lookup "):
		return Explained{ProblemDNS, "The device name does not resolve", "Use an IP address or fix DNS on the Zirox server", raw}
	case strings.Contains(low, "timeout") || strings.Contains(low, "max retries"):
		hint := "Check that the device ACL allows SNMP from " + from + " and that UDP 161 is open on firewalls on the way."
		if version == "3" {
			hint += " Also check the v3 user and engine settings"
		} else {
			hint += " A wrong community is silently ignored by most devices, so check it too"
		}
		return Explained{ProblemTimeout, "No answer from the device", hint, raw}
	case strings.Contains(low, "unknown auth") || strings.Contains(low, "unknown privacy") || strings.Contains(low, "required"):
		return Explained{ProblemConfig, "The credential settings are incomplete", "Open the credentials and fill in the missing field", raw}
	}
	return Explained{ProblemUnknown, raw, "", ""}
}

// CommunityWarning returns a warning for community strings that devices
// commonly misread, or "".
func CommunityWarning(c string) string {
	switch {
	case strings.Contains(c, "@"):
		return "The community contains '@'. Cisco devices read 'name@context' as community 'name' with SNMP context 'context', so polls fail with authorizationError or return the wrong data. Use a community without '@' if you can."
	case strings.TrimSpace(c) != c:
		return "The community starts or ends with a space; this is almost always a copy-paste mistake."
	}
	return ""
}

// SourceAddress is the local address the server uses to reach address, as
// device ACLs must allow it. No packet is sent. Returns "" when unknown.
func SourceAddress(address string) string {
	c, err := net.Dial("udp", net.JoinHostPort(address, "161"))
	if err != nil {
		return ""
	}
	defer c.Close()
	if a, ok := c.LocalAddr().(*net.UDPAddr); ok {
		return a.IP.String()
	}
	return ""
}
