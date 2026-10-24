package smb

import (
	"strings"
	"testing"
)

func TestSMB2Credential(t *testing.T) {
	cases := []struct {
		name         string
		u, p, d      string
		wantU, wantP string
		wantDom      *string // nil means server TargetName
	}{
		{"all set", "alice", "pw", "CORP", "alice", "pw", strp("CORP")},
		{"domain empty", "alice", "pw", "", "alice", "pw", nil},
		{"domain unset", "alice", "pw", krUnset, "alice", "pw", nil},
		{"password empty", "alice", "", "CORP", "alice", "", strp("CORP")},
		{"user unset", krUnset, "pw", "CORP", "", "pw", strp("CORP")},
		{"password unset", "alice", krUnset, krUnset, "alice", "", nil},
		{"anonymous empty", "", "", "", "", "", nil},
		{"anonymous unset", krUnset, krUnset, krUnset, "", "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := smb2Credential("nas", c.u, c.p, c.d)
			if got.User != c.wantU || got.Password != c.wantP {
				t.Fatalf("user/password mismatch")
			}
			if (got.Domain == nil) != (c.wantDom == nil) {
				t.Fatalf("domain nil-ness: got nil=%v want nil=%v",
					got.Domain == nil, c.wantDom == nil)
			}
			if c.wantDom != nil && *got.Domain != *c.wantDom {
				t.Fatalf("domain = %q, want %q", *got.Domain, *c.wantDom)
			}
			if got.Hash != nil {
				t.Fatalf("unexpected hash")
			}
		})
	}
}

// The target SPN is cifs/<host> with no port, whatever port is dialled.
func TestSMB2CredentialTargetSPN(t *testing.T) {
	cases := []struct{ name, host, want string }{
		{"hostname", "nas", "cifs/nas"},
		{"fqdn", "nas.example.lan", "cifs/nas.example.lan"},
		{"ipv4", "192.0.2.10", "cifs/192.0.2.10"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := smb2Credential(c.host, "u", "p", "")
			if got.TargetSPN == nil {
				t.Fatal("TargetSPN is nil: go-smb2 would derive cifs/<host>:<port>")
			}
			if *got.TargetSPN != c.want {
				t.Fatalf("TargetSPN = %q, want %q", *got.TargetSPN, c.want)
			}
			if strings.Contains(*got.TargetSPN, ":") {
				t.Fatalf("TargetSPN %q contains a port", *got.TargetSPN)
			}
		})
	}
}

func strp(s string) *string { return &s }
