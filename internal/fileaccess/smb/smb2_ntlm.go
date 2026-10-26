// Lenient NTLMv2 auth.Initiator for go-smb2. The library's bundled
// NTLM stack hardcodes NTLMSSP_NEGOTIATE_SIGN in its request flags and
// rejects challenges that do not echo SIGN / REQUEST_TARGET /
// TARGET_INFO — real-world minimal SMB2 servers (embedded stacks,
// modem/NAS firmware) fail against it. This initiator negotiates a
// permissive flag set and parses the challenge tolerantly while still
// producing a spec-correct NTLMv2 AUTHENTICATE_MESSAGE (NTProofStr,
// MIC). KEY_EXCH is never requested: per MS-NLMP 3.1.5.2.1 the
// exchanged session key is adopted only when KEY_EXCH *and* SIGN/SEAL
// are negotiated, and this client signs nothing — asking for it only
// desynced the session key on conformant servers, which kept the
// key-exchange key while we signed with a random one.
package smb

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/hirochachacha/go-smb2/v2/auth"
)

var ntlmOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 311, 2, 2, 10}

// NTLMSSP flag bits (MS-NLMP 2.2.2.5).
const (
	nfUNICODE    = 0x00000001
	nfOEM        = 0x00000002
	nfREQUESTTGT = 0x00000004
	nfSIGN       = 0x00000010
	nfSEAL       = 0x00000020
	nfNTLM       = 0x00000200
	nfALWAYSSIGN = 0x00008000
	nfTGTDOMAIN  = 0x00010000
	nfTGTSERVER  = 0x00020000
	nfESS        = 0x00080000
	nfTARGETINFO = 0x00800000
	nfVERSION    = 0x02000000
	nfKEYEXCH    = 0x40000000
	nf56         = 0x80000000
	nf128        = 0x20000000
)

// Flags we ask for: everything needed for NTLMv2, no
// SIGN/SEAL/VERSION/KEY_EXCH — servers that do not support NTLM
// signing still authenticate (the SMB layer decides signing
// independently).
const ntlmClientFlags = nfUNICODE | nfREQUESTTGT | nfNTLM |
	nfALWAYSSIGN | nfESS | nfTARGETINFO | nf56 | nf128

// smb2NTLMCredential adapts plaintext credentials to smb2.Credentials.
type smb2NTLMCredential struct {
	User     string
	Password string
	Domain   string // "" → use the server's challenge TargetName
}

func (c smb2NTLMCredential) NewInitiator(_ context.Context, _ string) (auth.Initiator, error) {
	return &smb2NTLMInitiator{creds: c}, nil
}

// smb2NTLMInitiator implements auth.Initiator (+ anonymousInitiator).
type smb2NTLMInitiator struct {
	creds smb2NTLMCredential

	neg, chal, auth []byte // kept for MIC computation
	exportedKey     []byte
	sendSeq         uint32
	recvSeq         uint32
	complete        bool
}

func (i *smb2NTLMInitiator) OID() asn1.ObjectIdentifier { return ntlmOID }

// IsAnonymous — anonymous sessions cannot sign; the protocol layer
// checks this optional interface.
func (i *smb2NTLMInitiator) IsAnonymous() bool {
	return i.creds.User == "" && i.creds.Password == ""
}

func utf16le(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2*len(u))
	for j, c := range u {
		binary.LittleEndian.PutUint16(b[2*j:], c)
	}
	return b
}

func hmacMD5(key, msg []byte) []byte {
	h := hmac.New(md5.New, key)
	h.Write(msg)
	return h.Sum(nil)
}

// putSecBuf writes a security-buffer descriptor (len, maxlen, offset).
func putSecBuf(b []byte, length, offset int) {
	binary.LittleEndian.PutUint16(b[0:], uint16(length))
	binary.LittleEndian.PutUint16(b[2:], uint16(length))
	binary.LittleEndian.PutUint32(b[4:], uint32(offset))
}

func getSecBuf(b []byte) (length, offset int) {
	return int(binary.LittleEndian.Uint16(b[0:])),
		int(binary.LittleEndian.Uint32(b[4:]))
}

// InitSecContext — NTLMSSP NEGOTIATE_MESSAGE (type 1).
func (i *smb2NTLMInitiator) InitSecContext() ([]byte, error) {
	i.sendSeq, i.recvSeq, i.complete = 0, 0, false
	dom := utf16le(i.creds.Domain)
	msg := make([]byte, 32+len(dom))
	copy(msg[0:8], "NTLMSSP\x00")
	binary.LittleEndian.PutUint32(msg[8:], 1)
	binary.LittleEndian.PutUint32(msg[12:], ntlmClientFlags)
	putSecBuf(msg[16:], len(dom), 32) // domain
	putSecBuf(msg[24:], 0, 32)        // workstation
	copy(msg[32:], dom)
	i.neg = msg
	return msg, nil
}

// AcceptSecContext — parse CHALLENGE_MESSAGE tolerantly, emit
// AUTHENTICATE_MESSAGE (type 3).
func (i *smb2NTLMInitiator) AcceptSecContext(cmsg []byte) ([]byte, error) {
	if i.complete || i.neg == nil {
		return nil, errors.New("ntlm: unexpected authentication token")
	}
	i.chal = cmsg
	if len(cmsg) < 48 || string(cmsg[:8]) != "NTLMSSP\x00" ||
		binary.LittleEndian.Uint32(cmsg[8:]) != 2 {
		return nil, errors.New("ntlm: malformed challenge message")
	}
	serverFlags := binary.LittleEndian.Uint32(cmsg[20:])
	if serverFlags&nfESS == 0 {
		return nil, errors.New("ntlm: server does not offer NTLMv2 " +
			"(extended session security)")
	}
	serverChallenge := cmsg[24:32]

	// Optional fields — do not require REQUEST_TARGET / TARGET_INFO.
	var targetName string
	if serverFlags&nfREQUESTTGT != 0 {
		if n, off := getSecBuf(cmsg[12:]); n > 0 && off+n <= len(cmsg) {
			targetName = utf16be2str(cmsg[off : off+n])
		}
	}
	var targetInfo []byte
	if n, off := getSecBuf(cmsg[40:]); n > 0 && off+n <= len(cmsg) {
		targetInfo = append([]byte(nil), cmsg[off:off+n]...)
	}

	// NegotiateFlags for the AUTHENTICATE: impacket-masked — strip
	// OEM/SIGN/SEAL/OEM-domain-supplied and always set REQUEST_TARGET.
	// Strip KEY_EXCH (never negotiated) and VERSION (the message
	// carries no Version field, so the flag must not claim one).
	t3flags := (serverFlags | nfREQUESTTGT) &^
		(nfOEM | nfSIGN | nfSEAL | nfVERSION | nfKEYEXCH | 0x00010000)

	// Anonymous: real clients send an AUTHENTICATE_MESSAGE with a
	// 1-byte null LM response and no NT response — not an NTLMv2
	// computed over an empty password. The anonymous KeyExchangeKey is
	// Z(16) and no key exchange happens, so the exported session key
	// is all zeroes.
	if i.IsAnonymous() {
		i.exportedKey = make([]byte, 16)
		msg := ntlmAuthMsg([]byte{0}, nil, nil, nil, nil, nil, t3flags)
		i.auth = msg
		i.complete = true
		return msg, nil
	}

	domain := i.creds.Domain
	if domain == "" {
		domain = targetName
	}

	// NTLMv2 response (MS-NLMP 3.3.2).
	nth := ntlmHash(i.creds.Password)
	ntowfv2 := hmacMD5(nth[:],
		utf16le(strings.ToUpper(i.creds.User)+domain))
	var clientNonce [8]byte
	rand.Read(clientNonce[:])

	// temp = 0x01010000 || 4z || timestamp || nonce || 4z ||
	//        targetInfo || 4z  — timestamp: server's MsvAvTimestamp
	//        when offered, else current time as FILETIME.
	ts := ntlmTimestamp(targetInfo)
	temp := make([]byte, 0, 28+len(targetInfo)+4)
	temp = append(temp, 0x01, 0x01, 0x00, 0x00, 0, 0, 0, 0)
	temp = binary.LittleEndian.AppendUint64(temp, ts)
	temp = append(temp, clientNonce[:]...)
	temp = append(temp, 0, 0, 0, 0)
	temp = append(temp, targetInfo...)
	temp = append(temp, 0, 0, 0, 0)

	ntProof := hmacMD5(ntowfv2,
		append(append([]byte{}, serverChallenge...), temp...))
	ntResp := append(append([]byte{}, ntProof...), temp...)
	lmResp := hmacMD5(ntowfv2,
		append(append([]byte{}, serverChallenge...), clientNonce[:]...))
	lmResp = append(lmResp, clientNonce[:]...)

	// With KEY_EXCH not negotiated, ExportedSessionKey is the
	// key-exchange key itself (MS-NLMP 3.1.5.2.1) — the session base
	// key for NTLMv2 — matching what the server derives. A server that
	// echoes KEY_EXCH+SIGN/SEAL unasked would expect an
	// EncryptedRandomSessionKey we do not send; such an echo is
	// already off-spec since we never offer those flags.
	i.exportedKey = hmacMD5(ntowfv2, ntProof)

	// AUTHENTICATE_MESSAGE: 64-byte header + 16-byte MIC + payload.
	msg := ntlmAuthMsg(lmResp, ntResp, utf16le(domain),
		utf16le(i.creds.User), nil, nil, t3flags, 16)

	i.auth = msg
	// MIC = HMAC-MD5(exportedKey, neg||chal||auth zeroed-MIC)
	mic := hmacMD5(i.exportedKey,
		append(append(append([]byte{}, i.neg...), i.chal...), msg...))
	copy(msg[64:80], mic)

	i.complete = true
	return msg, nil
}

// ntlmAuthMsg builds an AUTHENTICATE_MESSAGE body from wire-encoded
// fields.
func ntlmAuthMsg(lm, nt, dom, user, wk, key []byte,
	flags uint32, micLen ...int) []byte {
	ml := 0
	if len(micLen) > 0 {
		ml = micLen[0]
	}
	msg := make([]byte, 64+ml+len(lm)+len(nt)+len(dom)+len(user)+
		len(wk)+len(key))
	copy(msg[0:8], "NTLMSSP\x00")
	binary.LittleEndian.PutUint32(msg[8:], 3)
	off := 64 + ml
	put := func(f []byte, data []byte) {
		putSecBuf(f, len(data), off)
		copy(msg[off:], data)
		off += len(data)
	}
	put(msg[12:], lm)
	put(msg[20:], nt)
	put(msg[28:], dom)
	put(msg[36:], user)
	put(msg[44:], wk)
	put(msg[52:], key)
	binary.LittleEndian.PutUint32(msg[60:], flags)
	return msg
}

func (i *smb2NTLMInitiator) Complete() bool { return i.complete }

func (i *smb2NTLMInitiator) SessionKey() []byte {
	return append([]byte(nil), i.exportedKey...)
}

// ntlmSigningKey — MS-NLMP 3.4.5.2 SIGNKEY under extended session
// security: MD5(ExportedSessionKey || direction magic). KEY_EXCH is
// never negotiated, so message checksums are sent unsealed.
func (i *smb2NTLMInitiator) ntlmSigningKey(toServer bool) []byte {
	magic := "session key to server-to-client signing key magic constant\x00"
	if toServer {
		magic = "session key to client-to-server signing key magic constant\x00"
	}
	h := md5.New()
	h.Write(i.exportedKey)
	h.Write([]byte(magic))
	return h.Sum(nil)
}

// GetMIC — NTLM signature block for the SPNEGO mechListMIC (MS-NLMP
// 2.2.2.9.1): Version(4)=1 || HMAC-MD5(signingKey, seq||msg)[:8] ||
// SeqNum(4). Anonymous sessions share no verifiable key — real clients
// send no MIC, so neither do we.
func (i *smb2NTLMInitiator) GetMIC(message []byte) ([]byte, error) {
	if i.IsAnonymous() {
		return nil, nil
	}
	if !i.complete || i.exportedKey == nil {
		return nil, errors.New("ntlm: authentication is incomplete")
	}
	mic := make([]byte, 16)
	binary.LittleEndian.PutUint32(mic[0:], 1)
	ck := hmacMD5(i.ntlmSigningKey(true),
		append(binary.LittleEndian.AppendUint32([]byte{}, i.sendSeq),
			message...))
	copy(mic[4:12], ck[:8])
	binary.LittleEndian.PutUint32(mic[12:], i.sendSeq)
	i.sendSeq++
	return mic, nil
}

// VerifyMIC — verify the server's mechListMIC signature. Anonymous
// sessions have no shared key to verify against; accept whatever the
// server sends.
func (i *smb2NTLMInitiator) VerifyMIC(message, mic []byte) error {
	if i.IsAnonymous() {
		return nil
	}
	if !i.complete || i.exportedKey == nil {
		return errors.New("ntlm: authentication is incomplete")
	}
	if len(mic) < 12 {
		return errors.New("ntlm: short MIC")
	}
	ck := hmacMD5(i.ntlmSigningKey(false),
		append(binary.LittleEndian.AppendUint32([]byte{}, i.recvSeq),
			message...))
	if !hmac.Equal(mic[4:12], ck[:8]) {
		return errors.New("ntlm: invalid mechanism list MIC")
	}
	i.recvSeq++
	return nil
}

// utf16be2str decodes a UTF-16LE byte range (challenge strings are
// Unicode when NEGOTIATE_UNICODE is set).
func utf16be2str(b []byte) string {
	u := make([]uint16, len(b)/2)
	for j := range u {
		u[j] = binary.LittleEndian.Uint16(b[2*j:])
	}
	return string(utf16.Decode(u))
}

// ntlmTimestamp picks MsvAvTimestamp (avId 7) from the challenge
// targetInfo when present, else returns the current time as FILETIME.
func ntlmTimestamp(targetInfo []byte) uint64 {
	for off := 0; off+4 <= len(targetInfo); {
		avId := binary.LittleEndian.Uint16(targetInfo[off:])
		avLen := int(binary.LittleEndian.Uint16(targetInfo[off+2:]))
		if avId == 0 || off+4+avLen > len(targetInfo) {
			break
		}
		if avId == 7 && avLen == 8 {
			return binary.LittleEndian.Uint64(targetInfo[off+4:])
		}
		off += 4 + avLen
	}
	// FILETIME = 100ns ticks since 1601-01-01.
	return uint64(time.Now().Unix()+11644473600) * 10000000
}
