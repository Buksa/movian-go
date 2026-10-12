package httpcontrol

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	httpnet "github.com/czz/movian-go/internal/networking/http"
)

func TestHTTPControlRootPreservesConfiguredInstallerToken(t *testing.T) {
	server := httpnet.NewHTTPServer()
	handler := NewHTTPControlHandler(nil, "", "", "", "", "", "")
	token := `a&b"<>`
	handler.SetAuthToken(token)
	handler.Register(server)
	if err := server.Listen(0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)

	fetch := func(requestURL string) (int, string) {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, requestURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Close = true
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, string(body)
	}
	apiURL := fmt.Sprintf("http://127.0.0.1:%d", server.GetPort())
	status, body := fetch(apiURL + "/?token=" + url.QueryEscape(token))
	if status != http.StatusOK {
		t.Fatalf("root response = %d, want HTTP 200", status)
	}
	encodedToken := url.Values{"token": []string{token}}.Encode()
	wantLink := `href="/api/plugin/install?` + encodedToken + `"`
	if !strings.Contains(body, wantLink) {
		t.Fatalf("root installer link = %q, want %q", body, wantLink)
	}

	status, body = fetch(apiURL + "/?token=wrong")
	if status != http.StatusOK || strings.Contains(body, `href="/api/plugin/install?`) {
		t.Fatalf("invalid token was propagated: status=%d body=%q", status, body)
	}

	status, body = fetch(apiURL + "/api/plugin/install?token=" + url.QueryEscape(token))
	if status != http.StatusOK || !strings.Contains(body, "new URLSearchParams(window.location.search).get('token')") {
		t.Fatalf("installer page token workflow = (%d, %q)", status, body)
	}

	handler.SetAuthToken("")
	status, body = fetch(apiURL + "/")
	if status != http.StatusOK || !strings.Contains(body, `href="/api/plugin/install"`) ||
		strings.Contains(body, `href="/api/plugin/install?`) {
		t.Fatalf("root link without configured token = (%d, %q)", status, body)
	}
}
