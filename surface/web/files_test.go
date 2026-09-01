package web

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cat3399/pi-go/internal/app"
)

func TestFileGitAndIndexHTTPContracts(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "sample.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runWebTestGit(t, root, "init", "-b", "main")
	runWebTestGit(t, root, "add", "sample.txt")
	runWebTestGit(t, root, "-c", "user.name=Pi Go Test", "-c", "user.email=pi-go@example.invalid", "commit", "-m", "initial")
	if err := os.WriteFile(filepath.Join(root, "sample.txt"), []byte("hello\nchanged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := testService(t, app.ProductionConfig{
		WorkingDir: root, AgentDir: filepath.Join(t.TempDir(), "agent"), Environment: []string{},
	}, nil)
	server, err := New(Options{Version: "test", Application: service})
	if err != nil {
		t.Fatal(err)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	encodedRoot := encodeTestFilePath(realRoot)
	encodedFile := encodeTestFilePath(filepath.Join(realRoot, "sample.txt"))

	listed := serveWebAPIRequest(t, server, http.MethodGet, "/api/v1/files/"+encodedRoot+"?type=list", nil, "")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"name":"sample.txt"`) || strings.Contains(listed.Body.String(), `"name":".git"`) {
		t.Fatalf("list response = %d %s", listed.Code, listed.Body.String())
	}
	read := serveWebAPIRequest(t, server, http.MethodGet, "/api/v1/files/"+encodedFile+"?type=read", nil, "")
	if read.Code != http.StatusOK || !strings.Contains(read.Body.String(), `"content":"hello\nchanged\n"`) || !strings.Contains(read.Body.String(), `"language":"text"`) {
		t.Fatalf("read response = %d %s", read.Code, read.Body.String())
	}
	rangeRequest := httptest.NewRequest(http.MethodGet, "/api/v1/files/"+encodedFile+"?type=download", nil)
	rangeRequest.Host = "localhost"
	rangeRequest.Header.Set("Range", "bytes=1-3")
	rangeResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(rangeResponse, rangeRequest)
	if rangeResponse.Code != http.StatusPartialContent || rangeResponse.Body.String() != "ell" ||
		rangeResponse.Header().Get("Content-Range") != "bytes 1-3/14" || rangeResponse.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("range response = %d %q %#v", rangeResponse.Code, rangeResponse.Body.String(), rangeResponse.Header())
	}

	check := serveWebAPIRequest(t, server, http.MethodPost, "/api/v1/files/"+encodedRoot+"?type=upload-check", []byte(`{"fileNames":["sample.txt","new.txt"]}`), "application/json")
	if check.Code != http.StatusOK || !strings.Contains(check.Body.String(), `"conflicts":["sample.txt"]`) {
		t.Fatalf("upload check = %d %s", check.Code, check.Body.String())
	}
	var uploadBody bytes.Buffer
	multipartWriter := multipart.NewWriter(&uploadBody)
	part, err := multipartWriter.CreateFormFile("files", "new.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(part, "uploaded\n"); err != nil {
		t.Fatal(err)
	}
	if err := multipartWriter.Close(); err != nil {
		t.Fatal(err)
	}
	upload := serveWebAPIRequest(t, server, http.MethodPost, "/api/v1/files/"+encodedRoot+"?type=upload&conflict=error", uploadBody.Bytes(), multipartWriter.FormDataContentType())
	if upload.Code != http.StatusOK || !strings.Contains(upload.Body.String(), `"uploaded":["new.txt"]`) {
		t.Fatalf("upload response = %d %s", upload.Code, upload.Body.String())
	}
	deletePath := encodeTestFilePath(filepath.Join(realRoot, "new.txt"))
	deleted := serveWebAPIRequest(t, server, http.MethodDelete, "/api/v1/files/"+deletePath, nil, "")
	if deleted.Code != http.StatusOK || !strings.Contains(deleted.Body.String(), `"ok":true`) {
		t.Fatalf("delete response = %d %s", deleted.Code, deleted.Body.String())
	}
	if _, err := os.Stat(filepath.Join(realRoot, "new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted upload still exists: %v", err)
	}

	statusTarget := "/api/v1/git/status?cwd=" + url.QueryEscape(realRoot)
	status := serveWebAPIRequest(t, server, http.MethodGet, statusTarget, nil, "")
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"isGitRepository":true`) || !strings.Contains(status.Body.String(), `"status":"modified"`) {
		t.Fatalf("git status = %d %s", status.Code, status.Body.String())
	}
	indexTarget := "/api/v1/file-index?cwd=" + url.QueryEscape(realRoot) + "&q=samp"
	index := serveWebAPIRequest(t, server, http.MethodGet, indexTarget, nil, "")
	if index.Code != http.StatusOK || !strings.Contains(index.Body.String(), `"path":"sample.txt"`) {
		t.Fatalf("file index = %d %s", index.Code, index.Body.String())
	}
}

func TestDOCXPreviewUsesOriginalWrapperAndEscapesContent(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "example.docx")
	handle, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(handle)
	part, err := archive.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(part, `<w:document xmlns:w="urn:test"><w:body><w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:b/><w:t>&lt;Hello&gt;</w:t></w:r></w:p></w:body></w:document>`); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	preview, err := renderDOCXPreview(filePath, `<example>.docx`)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		`html, body { margin: 0; min-height: 100%; background: #eef1f5; color: #171717; }`,
		`<div class="file-title">&lt;example&gt;.docx</div>`,
		`<h1><strong>&lt;Hello&gt;</strong></h1>`,
	} {
		if !strings.Contains(preview, expected) {
			t.Fatalf("preview missing %q:\n%s", expected, preview)
		}
	}
}

func serveWebAPIRequest(t *testing.T, server *Server, method, target string, body []byte, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, bytes.NewReader(body))
	request.Host = "localhost"
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

func encodeTestFilePath(value string) string {
	parts := strings.Split(filepath.ToSlash(value), "/")
	encoded := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			encoded = append(encoded, url.PathEscape(part))
		}
	}
	return strings.Join(encoded, "/")
}

func runWebTestGit(t *testing.T, cwd string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", cwd}, arguments...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
}
