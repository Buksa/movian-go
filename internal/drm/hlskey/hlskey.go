// Package hlskey provides a cdm.Session implementation for HLS
// EXT-X-KEY identity keys (KEYFORMAT="identity"): the playlist URI
// returns the raw 16-byte AES-128 content key itself, so the session
// is usable immediately — no PSSH, no license exchange. This is the
// key-delivery bridge that lets the generic pipe.Decryptor /
// cenc.DecryptSample machinery handle METHOD=SAMPLE-AES-CTR streams
// (CMAF 'cenc'/'cens'/'cbcs' sample encryption on fMP4 segments).
package hlskey

import (
	"fmt"

	"github.com/czz/movian-go/internal/drm/cdm"
	"github.com/czz/movian-go/internal/drm/cenc"
)

// SystemID is a sentinel identifying sessions seeded by EXT-X-KEY
// identity keys. It does not match any ISO 23001-1 key system — it
// only namespaces the entry inside pipe.Decryptor's session map so a
// real CDM session for the same pipeline cannot collide with it.
var SystemID = [16]byte{
	'h', 'l', 's', '-', 'i', 'd', 'e', 'n',
	't', 'i', 't', 'y', '-', 'k', 'e', 'y',
}

// Session is an already-usable cdm.Session wrapping one raw AES-128
// content key fetched from the EXT-X-KEY URI.
type Session struct {
	key [16]byte
}

// NewSession returns a usable session. key must be exactly the 16 raw
// bytes the key URI returned (AES-128).
func NewSession(key []byte) (*Session, error) {
	if len(key) != 16 {
		return nil, fmt.Errorf("hlskey: key is %d bytes, want 16", len(key))
	}
	s := &Session{}
	copy(s.key[:], key)
	return s, nil
}

// LicenseRequest — identity sessions never need a license exchange.
func (s *Session) LicenseRequest() *cdm.LicenseRequest { return nil }

// Update — there is no license response to consume; already usable.
func (s *Session) Update(response []byte) error { return nil }

// Usable reports whether the content key is available.
func (s *Session) Usable() bool { return s != nil }

// DecryptSample decrypts the sample in place; all four CENC schemes
// are handled by the shared cenc layer (cenc/cbc1/cens/cbcs).
func (s *Session) DecryptSample(sample []byte, info *cenc.SampleInfo) error {
	if info == nil {
		return nil
	}
	return cenc.DecryptSample(sample, sample, s.key[:], info)
}

// Close zeroizes the key material.
func (s *Session) Close() {
	for i := range s.key {
		s.key[i] = 0
	}
}
