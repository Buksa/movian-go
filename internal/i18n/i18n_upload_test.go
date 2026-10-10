package i18n

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appcore "github.com/czz/movian-go/internal/app"
	httpnet "github.com/czz/movian-go/internal/networking/http"
)

func TestSetHTTPServerAfterStartServesTranslationUpload(t *testing.T) {
	dataRoot := t.TempDir()
	previousDataDir := appcore.DataDir
	appcore.DataDir = dataRoot
	t.Cleanup(func() { appcore.DataDir = previousDataDir })

	i18nInstance := NewI18N(nil, nil)
	t.Cleanup(i18nInstance.nlsClear)
	i18nInstance.Start()

	server := httpnet.NewHTTPServer()
	i18nInstance.SetHTTPServer(server)
	if err := server.Listen(0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)

	request, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/api/translation", server.GetPort()), nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Close = true
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET /api/translation: %v", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading upload form: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/translation = %d (%s), want %d", response.StatusCode, body, http.StatusOK)
	}
	if !strings.Contains(string(body), `<input type="file" name="langfile" accept=".lang">`) {
		t.Fatalf("upload form missing .lang file input: %s", body)
	}

	uploadData := "id: upload.test\nmsg: Uploaded translation\n"
	var uploadBody bytes.Buffer
	writer := multipart.NewWriter(&uploadBody)
	part, err := writer.CreateFormFile("langfile", "smoke.lang")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(part, uploadData); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	uploadRequest, err := http.NewRequest(
		http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/api/translation", server.GetPort()),
		&uploadBody,
	)
	if err != nil {
		t.Fatal(err)
	}
	uploadRequest.Header.Set("Content-Type", writer.FormDataContentType())
	uploadRequest.Close = true
	uploadResponse, err := http.DefaultClient.Do(uploadRequest)
	if err != nil {
		t.Fatalf("POST /api/translation: %v", err)
	}
	defer uploadResponse.Body.Close()

	uploadResult, err := io.ReadAll(uploadResponse.Body)
	if err != nil {
		t.Fatalf("reading upload result: %v", err)
	}
	if uploadResponse.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/translation = %d (%s), want %d", uploadResponse.StatusCode, uploadResult, http.StatusOK)
	}
	if !strings.Contains(string(uploadResult), "Translation Uploaded Successfully") {
		t.Fatalf("upload success page missing from response: %s", uploadResult)
	}

	saved, err := os.ReadFile(filepath.Join(dataRoot, "lang", "smoke.lang"))
	if err != nil {
		t.Fatalf("reading saved translation: %v", err)
	}
	if string(saved) != uploadData {
		t.Fatalf("saved translation = %q, want %q", saved, uploadData)
	}
	if got := i18nInstance.GetString("upload.test"); got != "Uploaded translation" {
		t.Fatalf("loaded translation = %q, want %q", got, "Uploaded translation")
	}
}
