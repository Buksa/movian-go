package smb

// Opt-in, read-only SMB2/3 integration smoke. A known, stable fixture on a
// disposable test share is required; ordinary `go test ./...` skips it.
// How to run it: support/smb-smoke-mgo/README.md.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	facore "github.com/czz/movian-go/internal/fileaccess"
)

type smbSmokeConfig struct {
	host, share, dir, file, user, password, domain, digest string
	port                                                   int
}

func smbSmokeEnv(t *testing.T) smbSmokeConfig {
	t.Helper()
	if os.Getenv("SMB_SMOKE_ENABLE") != "1" {
		t.Skip("opt-in remote smoke: set SMB_SMOKE_ENABLE=1")
	}
	env := func(key string) string {
		v := strings.TrimSpace(os.Getenv(key))
		if v == "" {
			t.Fatalf("%s is required", key)
		}
		return v
	}
	c := smbSmokeConfig{
		host:     env("SMB_SMOKE_HOST"),
		share:    env("SMB_SMOKE_SHARE"),
		file:     env("SMB_SMOKE_FILE"),
		digest:   strings.ToLower(env("SMB_SMOKE_SHA256")),
		user:     os.Getenv("SMB_SMOKE_USER"),
		password: os.Getenv("SMB_SMOKE_PASSWORD"),
		domain:   os.Getenv("SMB_SMOKE_DOMAIN"),
		dir:      strings.Trim(os.Getenv("SMB_SMOKE_PATH"), "/"),
		port:     445,
	}
	if v := os.Getenv("SMB_SMOKE_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil || p < 1 || p > 65535 {
			t.Fatal("SMB_SMOKE_PORT must be in 1..65535")
		}
		c.port = p
	}
	if len(c.digest) != 64 {
		t.Fatal("SMB_SMOKE_SHA256 must be the 64-character SHA-256 of the fixture")
	}
	if _, err := hex.DecodeString(c.digest); err != nil {
		t.Fatal("SMB_SMOKE_SHA256 is not hexadecimal")
	}
	if strings.Contains(c.file, "/") || c.file == "." || c.file == ".." {
		t.Fatal("SMB_SMOKE_FILE must be a basename in SMB_SMOKE_PATH")
	}
	if c.domain == "" {
		c.domain = "WORKGROUP"
	}
	return c
}

func (c smbSmokeConfig) baseURL() string {
	host := c.host
	if c.port != 445 {
		host = net.JoinHostPort(host, strconv.Itoa(c.port))
	}
	base := "smb://" + host + "/" + c.share
	if c.dir != "" {
		base += "/" + c.dir
	}
	return base
}

func (c smbSmokeConfig) hostURL() string {
	host := c.host
	if c.port != 445 {
		host = net.JoinHostPort(host, strconv.Itoa(c.port))
	}
	return "smb://" + host
}

func smbSmokeSystem(c smbSmokeConfig) *System {
	sys := &System{}
	sys.SetKeyringLookup(func(id string, user, password, domain *string,
		rememberMe *int, source, reason string, flags int) int {
		if user != nil {
			*user = c.user
		}
		if password != nil {
			*password = c.password
		}
		if domain != nil {
			*domain = c.domain
		}
		return 0
	})
	return sys
}

func smbSmokeClose(sys *System) {
	sys.s2mu.Lock()
	conns := make([]*smb2Conn, 0, len(sys.s2conns))
	for _, cc := range sys.s2conns {
		conns = append(conns, cc)
	}
	sys.s2conns = nil
	sys.s2mu.Unlock()
	for _, cc := range conns {
		cc.sess.Abort()
	}
}

func TestSMB2RemoteSmoke(t *testing.T) {
	c := smbSmokeEnv(t)
	sys := smbSmokeSystem(c)
	defer smbSmokeClose(sys)
	base := c.baseURL()
	fileURL := base + "/" + c.file

	if os.Getenv("SMB_SMOKE_SKIP_HOST_ENUM") != "1" {
		for _, suffix := range []string{"", "/"} {
			t.Run("host-enum-"+fmt.Sprint(len(suffix)), func(t *testing.T) {
				dir, err := sys.smbScandirGo(c.hostURL()+suffix, 0)
				if err != nil {
					t.Fatalf("SMB2 host enumeration: %v", err)
				}
				defer dir.Free()
				found := false
				for name := range dir.Entries {
					if strings.EqualFold(name, c.share) {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("share %q missing in host listing", c.share)
				}
			})
		}
	}

	t.Run("share-list", func(t *testing.T) {
		dir, err := sys.smbScandirGo(base+"/", 0)
		if err != nil {
			t.Fatalf("SMB2 share listing: %v", err)
		}
		defer dir.Free()
		if _, ok := dir.Entries[c.file]; !ok {
			t.Fatalf("expected fixture %q missing", c.file)
		}
	})

	t.Run("stat-read-seek-sha256", func(t *testing.T) {
		st, err := sys.smbStatGo(fileURL)
		if err != nil {
			t.Fatalf("SMB2 stat: %v", err)
		}
		if st.Type != facore.ContentFile || st.Size <= 0 {
			t.Fatalf("bad fixture stat: type=%v size=%d", st.Type, st.Size)
		}
		f, err := sys.smbOpenGo(fileURL, nil)
		if err != nil {
			t.Fatalf("SMB2 file open: %v", err)
		}
		defer f.Close()
		if f.Size() != st.Size {
			t.Fatalf("open size=%d stat size=%d", f.Size(), st.Size)
		}
		h := sha256.New()
		n, err := io.Copy(h, f)
		if err != nil {
			t.Fatalf("SMB2 sequential read: %v", err)
		}
		if n != st.Size {
			t.Fatalf("read %d bytes but stat size is %d", n, st.Size)
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != c.digest {
			t.Fatalf("fixture SHA256 mismatch: got %s want %s", got, c.digest)
		}
		// Verify actual re-seeking and read-after-seek, not just the return code.
		for _, off := range []int64{0, st.Size / 2, st.Size - 1} {
			got, err := f.Seek(off, io.SeekStart)
			if err != nil || got != off {
				t.Fatalf("seek(%d) => %d / %v", off, got, err)
			}
			var one [1]byte
			n, err := f.Read(one[:])
			if err != nil || n != 1 {
				t.Fatalf("read after seek(%d) => %d / %v", off, n, err)
			}
		}
	})

	t.Run("modern-session-used", func(t *testing.T) {
		sys.s2mu.Lock()
		defer sys.s2mu.Unlock()
		key := net.JoinHostPort(c.host, strconv.Itoa(c.port))
		if sys.s2down[key] {
			t.Fatal("unexpected SMB1 fallback recorded")
		}
		if sys.s2conns[key] == nil || sys.s2conns[key].dead {
			t.Fatal("expected an active pooled SMB2/3 session")
		}
	})

	// Deliberately opt in to concurrency stress; it may expose a session
	// replacement race in current upstream, which is useful regression evidence.
	if os.Getenv("SMB_SMOKE_STRESS") == "1" {
		t.Run("concurrent-opens", func(t *testing.T) {
			raceSys := smbSmokeSystem(c)
			defer smbSmokeClose(raceSys)
			const clients = 12
			var wg sync.WaitGroup
			results := make(chan error, clients)
			// Start barrier: every goroutine hits the cold pool at once.
			start := make(chan struct{})
			for range clients {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					f, err := raceSys.smbOpenGo(fileURL, nil)
					if err != nil {
						results <- err
						return
					}
					defer f.Close()
					var b [1]byte
					n, err := f.Read(b[:])
					if err == nil && n != 1 {
						err = fmt.Errorf("short read: %d", n)
					}
					results <- err
				}()
			}
			close(start)
			wg.Wait()
			close(results)
			for err := range results {
				if err != nil {
					t.Errorf("parallel open/read: %v", err)
				}
			}
		})
	}
}
