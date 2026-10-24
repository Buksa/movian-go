# SMB2/3 client smoke

Opt-in, read-only checks of the SMB2/3 client in `internal/fileaccess/smb`. SMB2 authentication is done by go-smb2's own `auth.NTLMCredential` (SIGN|KEY_EXCH|VERSION, real NTLM signing); MGO has no NTLM initiator of its own for SMB2. Adapted from the Movian `support/smb-smoke` suite. Plain `go test ./...` skips them.

- `internal/fileaccess/smb/smb2_smoke_test.go` (`TestSMB2RemoteSmoke`) goes through the real `smbScandirGo`, `smbStatGo` and `smbOpenGo` paths. It checks:
  - host enumeration of `smb://host` and `smb://host/`;
  - share listing;
  - stat, a full SHA-256 read and reads after seeks;
  - that the pooled SMB2/3 session was used and no SMBv1 fallback was recorded;
  - with `SMB_SMOKE_STRESS=1`, 12 concurrent opens on a cold pool.
- `internal/fileaccess/smb/smb2_cred_test.go` is an offline table test of the keyring-to-`auth.NTLMCredential` mapping. `Domain` must be nil, so the server's TargetName is used, exactly when the keyring domain is empty or unset.
- `fixture/` builds a disposable Samba image with SMB2/3 only, one read-only share `smoke` and one user `smoke`.
- `make-fixture.sh` writes the deterministic 1 MiB fixture file and prints its name and SHA-256.
- `run-client-smoke.sh` runs `TestSMB2RemoteSmoke` with `go test` and redacts the password from the log.
- `run-http-nav-smoke.sh` drives an already running, isolated Movian Go through HTTP Control on loopback. It checks that the share and the fixture show up in navigation.

## Fixture

```bash
mkdir -p ~/mgo-smb-smoke/share/test-only && cd ~/mgo-smb-smoke
support/smb-smoke-mgo/make-fixture.sh share/test-only/mgo-smoke-1mib.bin
chmod 755 share share/test-only && chmod 644 share/test-only/*.bin
( umask 077; python3 -c 'import secrets; print(secrets.token_urlsafe(18))' > smb_password ) && chmod 600 smb_password
docker build -t mgo-smb-fixture:test <checkout>/support/smb-smoke-mgo/fixture
docker run -d --name mgo-smb-smoke --tmpfs /run/samba -p 127.0.0.1:1445:445 \
  -v "$PWD/smb_password:/run/secrets/smb_password:ro" \
  -v "$PWD/share:/share:ro" mgo-smb-fixture:test
```

The password lives in the file `smb_password`, forced to mode 600 on every run (the `chmod` also fixes a pre-existing file, which the redirection alone would leave with its old mode) and reaches the container as a read-only mounted file, not through `--env-file` or `-e`, so `docker inspect` does not show it. The entrypoint reads `/run/secrets/smb_password` (override with `SMB_SMOKE_PASSWORD_FILE`) and feeds `smbpasswd` on stdin. To check, print only a count: `docker inspect mgo-smb-smoke | grep -c -F -f smb_password` must be `0`.

To pin one dialect, pass `-e SMB_FIXTURE_MAX_PROTOCOL=SMB2_02` to `docker run`. It also accepts `SMB2_10`, `SMB3_00`, `SMB3_02` and `SMB3_11`.

Host enumeration needs the srvsvc RPC service. The entrypoint starts `samba-dcerpcd` itself, because on-demand start fails inside containers. Under WSL2, `samba-dcerpcd` still cannot bind its pipe sockets (`Interrupted system call`), so there run with `SMB_SMOKE_SKIP_HOST_ENUM=1` or use a Linux host.

## Client smoke

Read the password from `smb_password`; never put it in a URL or on the command line. `run-client-smoke.sh` hands it to its log redactor through the environment, so no process carries it in argv.

```bash
export SMB_SMOKE_PASSWORD="$(cat ~/mgo-smb-smoke/smb_password)"
SMB_SMOKE_HOST=127.0.0.1 SMB_SMOKE_PORT=1445 SMB_SMOKE_SHARE=smoke \
SMB_SMOKE_PATH=test-only SMB_SMOKE_FILE=mgo-smoke-1mib.bin \
SMB_SMOKE_SHA256=<digest printed by make-fixture.sh> SMB_SMOKE_USER=smoke \
SMB_SMOKE_STRESS=1 support/smb-smoke-mgo/run-client-smoke.sh
```

On a host too small to build Movian Go, build the test binary elsewhere with `go test -c ./internal/fileaccess/smb`. Copy it over and run it there with `-test.v -test.run '^TestSMB2RemoteSmoke$'` and the same environment.

Optional variables:

- `SMB_SMOKE_DOMAIN` defaults to `WORKGROUP`.
- `SMB_SMOKE_SKIP_HOST_ENUM=1` skips host enumeration when the server does not allow it.

## HTTP Control navigation

Start your own Movian Go with a throwaway profile (`--persistent` and `--cache` under `/tmp`), and make sure it is the process that owns port 42000. HTTP Control listens on all interfaces without auth, so stop it after the run.

To skip the login dialog, pre-seed `<persistent>/settings/keyring` (`chmod 600` it after writing; an existing file keeps its old mode) with the test login. It is a JSON object `{"smb:connection:<host>:<port>": {"username": …, "password": …, "domain": …}}`; read the password from `smb_password` with a script, not by hand.

Directory pages hide plain files: "Show only supported files" is on by default. The `.bin` fixture therefore never shows up in navigation, so for this check put a small media file into `test-only` and pass its name as `SMB_SMOKE_FILE`. A one-second WAV written with Python's `wave` module is enough.

## Results

Run on 2026-10-23. Each cell is whether dial and mount work.

| Server | MGO's own initiator at `a13b944` | MGO's initiator after the KEY_EXCH/VERSION/Z(16) fixes | go-smb2 `auth.NTLMCredential` |
|---|---|---|---|
| Windows 11 (SMB1 disabled) | "session setup response failed signature verification" | same | works: dial, mount |
| NAS (Samba-based) | same | same | works |
| Samba 4.22 in docker (this fixture) | same | works | works |

Why the own initiator failed (MS-NLMP 3.1.5.1.2): key exchange happens only when KEY_EXCH and SIGN or SEAL are both negotiated. MGO never requested SIGN. Windows and the NAS ignore the EncryptedRandomSessionKey, so MGO's signing with the random key broke; Samba accepts KEY_EXCH alone, so the earlier fixes worked only there. Requesting SIGN gave Access Denied (MGO's `GetMIC` was not a real NTLM signature), and dropping KEY_EXCH gave STATUS_INVALID_PARAMETER on Windows and the NAS.

With go-smb2's initiator, against the Samba 4.22 fixture:

- `TestSMB2RemoteSmoke` with `SMB_SMOKE_STRESS=1`: 4 of 4 subtests passed (`share-list`, `stat-read-seek-sha256`, `modern-session-used`, `concurrent-opens`; host enumeration skipped under WSL2).
- A wrong password is classified as an auth error (`smb2IsAuthError` true, `smb2Dial` returns `ErrAuthRequired`), not `errSMB2Unsupported`.
- An anonymous dial and mount of a guest share works on a null-session config (`map to guest = Bad User`, `restrict anonymous = 0`, a share with `guest ok = yes`).
- Earlier navigation smoke: when the directory URL ends in `/`, the listed item URL has a double slash (`test-only//file.wav`). Stat and open still work with it.

Windows 11 and the NAS were checked separately, from another Linux host. An SMB2 auth error that is not a `protocol.ResponseError` is still wrapped as `errSMB2Unsupported` and falls back to SMBv1; that is a separate follow-up.

## Known limits

- go-smb2 requires the server to negotiate REQUEST_TARGET and TARGET_INFO in its NTLM challenge. Windows, Samba and the tested NAS do; a server that omits either now fails, where MGO's removed initiator tolerated it. Otherwise the local parse error is not a `protocol.ResponseError`, is classified as unsupported, and the client falls back to SMBv1. The classification is pre-existing; changing it is a follow-up.
- `STATUS_ACCOUNT_LOCKED_OUT` is not treated as an auth error by `smb2IsAuthError`, so a locked account does not trigger the credential retry. Pre-existing; follow-up.
