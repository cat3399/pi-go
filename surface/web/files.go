package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/surfacewire"
	"github.com/fsnotify/fsnotify"
)

const (
	textPreviewMaxBytes   = surfacewire.TextPreviewMaxBytes
	imagePreviewMaxBytes  = surfacewire.ImagePreviewMaxBytes
	docxPreviewMaxBytes   = surfacewire.DOCXPreviewMaxBytes
	maxUploadFileBytes    = 25 * 1024 * 1024
	maxUploadTotalBytes   = 100 * 1024 * 1024
	maxUploadRequestBytes = maxUploadTotalBytes + 1024*1024
)

var fileRequestTypes = map[string]struct{}{
	"list": {}, "read": {}, "download": {}, "meta": {}, "preview": {}, "watch": {},
}

var byteRangePattern = regexp.MustCompile(`^bytes=(\d*)-(\d*)$`)

func handleFileGet(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		filePath := filePathFromWildcard(request.PathValue("path"))
		requestType := request.URL.Query().Get("type")
		if requestType == "" {
			requestType = "list"
		}
		if _, valid := fileRequestTypes[requestType]; !valid {
			writeAPIError(writer, http.StatusBadRequest, errors.New("Invalid file request type"))
			return
		}
		if requestType == "list" {
			result, err := api.ListFiles(request.Context(), filePath)
			if err != nil {
				writeFileError(writer, err)
				return
			}
			writeJSON(writer, http.StatusOK, map[string]any{"entries": result.Entries, "path": result.Path})
			return
		}
		resource, err := api.ResolveFile(request.Context(), filePath, request.URL.Query().Get("sessionId"))
		if err != nil {
			writeFileError(writer, err)
			return
		}
		if !resource.IsFile {
			writeAPIError(writer, http.StatusBadRequest, errors.New("Not a file"))
			return
		}
		switch requestType {
		case "read":
			handleFileRead(writer, request, resource)
		case "download":
			streamFileResponse(writer, request, resource, fileMIME(resource.Path), true)
		case "meta":
			previewKind := any(nil)
			if extension := fileExtension(resource.Path); extension == "pdf" || extension == "docx" {
				previewKind = extension
			}
			mimeType := fileMIME(resource.Path)
			if mimeType == "application/octet-stream" {
				mimeType = "text/plain"
			}
			writeJSON(writer, http.StatusOK, map[string]any{
				"size": resource.Size, "language": fileLanguage(resource.Path), "mime": mimeType, "previewKind": previewKind,
			})
		case "preview":
			handleDocumentPreview(writer, resource)
		case "watch":
			handleFileWatch(writer, request, resource)
		}
	}
}

func handleFileRead(writer http.ResponseWriter, request *http.Request, resource application.FileResource) {
	if mimeType := imageMIME(resource.Path); mimeType != "" {
		if resource.Size > imagePreviewMaxBytes {
			writeAPIError(writer, http.StatusRequestEntityTooLarge, errors.New("Image too large (>10MB)"))
			return
		}
		streamFileResponse(writer, request, resource, mimeType, false)
		return
	}
	if mimeType := audioMIME(resource.Path); mimeType != "" {
		streamFileResponse(writer, request, resource, mimeType, false)
		return
	}
	if mimeType := documentMIME(resource.Path); mimeType != "" {
		streamFileResponse(writer, request, resource, mimeType, false)
		return
	}
	if resource.Size > textPreviewMaxBytes {
		writeAPIError(writer, http.StatusRequestEntityTooLarge, errors.New("File too large for preview (>256KB)"))
		return
	}
	content, err := os.ReadFile(resource.Path)
	if err != nil {
		writeAPIError(writer, http.StatusInternalServerError, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"content": string(content), "language": fileLanguage(resource.Path), "size": resource.Size,
	})
}

func streamFileResponse(writer http.ResponseWriter, request *http.Request, resource application.FileResource, contentType string, asDownload bool) {
	handle, err := os.Open(resource.Path)
	if err != nil {
		writeFileError(writer, err)
		return
	}
	defer handle.Close()
	info, err := handle.Stat()
	if err != nil || !info.Mode().IsRegular() {
		if err == nil {
			err = application.ErrNotFile
		}
		writeFileError(writer, err)
		return
	}
	size := info.Size()
	disposition := "inline"
	if asDownload {
		disposition = "attachment"
	}
	writer.Header().Set("Content-Type", contentType)
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	writer.Header().Set("Accept-Ranges", "bytes")
	writer.Header().Set("Content-Disposition", contentDisposition(disposition, resource.Name))
	rangeHeader := request.Header.Get("Range")
	if rangeHeader == "" {
		writer.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		writer.WriteHeader(http.StatusOK)
		if request.Method != http.MethodHead {
			_, _ = io.Copy(writer, handle)
		}
		return
	}
	start, end, valid := parseByteRange(rangeHeader, size)
	if !valid {
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		writer.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	length := end - start + 1
	writer.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	writer.WriteHeader(http.StatusPartialContent)
	if request.Method == http.MethodHead {
		return
	}
	if _, err := handle.Seek(start, io.SeekStart); err == nil {
		_, _ = io.CopyN(writer, handle, length)
	}
}

func parseByteRange(value string, size int64) (int64, int64, bool) {
	match := byteRangePattern.FindStringSubmatch(value)
	if match == nil {
		return 0, 0, false
	}
	start, end := int64(0), size-1
	var err error
	if match[1] != "" {
		start, err = strconv.ParseInt(match[1], 10, 64)
		if err != nil {
			return 0, 0, false
		}
	}
	if match[2] != "" {
		end, err = strconv.ParseInt(match[2], 10, 64)
		if err != nil {
			return 0, 0, false
		}
	}
	if match[1] == "" && match[2] != "" {
		suffixLength := end
		start = max(size-suffixLength, 0)
		end = size - 1
	}
	if start < 0 || end < start || start >= size {
		return 0, 0, false
	}
	if end >= size {
		end = size - 1
	}
	return start, end, true
}

func handleDocumentPreview(writer http.ResponseWriter, resource application.FileResource) {
	if fileExtension(resource.Path) != "docx" {
		writeAPIError(writer, http.StatusBadRequest, errors.New("Preview not available for this file type"))
		return
	}
	if resource.Size > docxPreviewMaxBytes {
		writeAPIError(writer, http.StatusRequestEntityTooLarge, errors.New("DOCX too large for preview (>10MB)"))
		return
	}
	html, err := renderDOCXPreview(resource.Path, resource.Name)
	if err != nil {
		writeAPIError(writer, http.StatusInternalServerError, err)
		return
	}
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Content-Security-Policy", "default-src 'none'; img-src data:; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'self'")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(writer, html)
}

func handleFileWatch(writer http.ResponseWriter, request *http.Request, resource application.FileResource) {
	flusher, ok := writer.(http.Flusher)
	if !ok {
		writeAPIError(writer, http.StatusInternalServerError, errors.New("streaming is unavailable"))
		return
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		writeAPIError(writer, http.StatusInternalServerError, err)
		return
	}
	defer watcher.Close()
	if err := watcher.Add(resource.Path); err != nil {
		writeAPIError(writer, http.StatusInternalServerError, errors.New("Failed to watch file"))
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache, no-transform")
	writer.Header().Set("Connection", "keep-alive")
	writer.Header().Set("X-Accel-Buffering", "no")
	writeNamedSSE(writer, "connected", map[string]any{"filePath": resource.Path})
	flusher.Flush()
	lastModified, lastSize := resource.Modified, resource.Size
	for {
		select {
		case <-request.Context().Done():
			return
		case _, open := <-watcher.Errors:
			if !open {
				return
			}
			return
		case _, open := <-watcher.Events:
			if !open {
				return
			}
			info, statErr := os.Stat(resource.Path)
			if statErr != nil {
				writeNamedSSE(writer, "change", map[string]any{"mtime": isoTime(time.Now()), "size": 0})
				flusher.Flush()
				continue
			}
			if info.ModTime().Equal(lastModified) && info.Size() == lastSize {
				continue
			}
			lastModified, lastSize = info.ModTime(), info.Size()
			writeNamedSSE(writer, "change", map[string]any{"mtime": isoTime(lastModified), "size": lastSize})
			flusher.Flush()
		}
	}
}

func writeNamedSSE(writer io.Writer, event string, value any) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", event, encoded)
}

func handleFileUpload(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		directory := filePathFromWildcard(request.PathValue("path"))
		requestType := request.URL.Query().Get("type")
		if requestType == "" {
			requestType = "upload"
		}
		if requestType == "upload-check" {
			handleUploadCheck(writer, request, api, directory)
			return
		}
		if requestType != "upload" {
			writeAPIError(writer, http.StatusBadRequest, errors.New("Invalid upload request type"))
			return
		}
		strategy := application.UploadConflictStrategy(request.URL.Query().Get("conflict"))
		if strategy == "" {
			strategy = application.UploadConflictError
		}
		if strategy != application.UploadConflictError && strategy != application.UploadConflictOverwrite && strategy != application.UploadConflictSkip {
			writeAPIError(writer, http.StatusBadRequest, errors.New("Invalid conflict strategy"))
			return
		}
		files, ok := readMultipartUploads(writer, request)
		if !ok {
			return
		}
		result, err := api.SaveUploads(request.Context(), directory, files, strategy)
		if errors.Is(err, application.ErrUploadConflict) {
			writeJSON(writer, http.StatusConflict, map[string]any{
				"error": "One or more files already exist", "conflicts": result.Inspection.Conflicts, "nonReplaceable": result.Inspection.NonReplaceable,
			})
			return
		}
		if err != nil {
			writeUploadError(writer, err)
			return
		}
		status := http.StatusOK
		if len(result.Errors) != 0 {
			status = http.StatusMultiStatus
		}
		writeJSON(writer, status, map[string]any{"uploaded": result.Uploaded, "skipped": result.Skipped, "errors": result.Errors})
	}
}

func handleUploadCheck(writer http.ResponseWriter, request *http.Request, api application.API, directory string) {
	body, err := readRequestBody(writer, request)
	if err != nil {
		writeRequestBodyError(writer, err)
		return
	}
	var input struct {
		FileNames []string `json:"fileNames"`
	}
	if err := json.Unmarshal(body, &input); err != nil || input.FileNames == nil {
		writeAPIError(writer, http.StatusBadRequest, errors.New("fileNames must be an array of strings"))
		return
	}
	inspection, err := api.InspectUploadTargets(request.Context(), directory, input.FileNames)
	if err != nil {
		writeUploadError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, inspection)
}

func readMultipartUploads(writer http.ResponseWriter, request *http.Request) ([]application.UploadFile, bool) {
	request.Body = http.MaxBytesReader(writer, request.Body, maxUploadRequestBytes)
	if err := request.ParseMultipartForm(32 << 20); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) || strings.Contains(err.Error(), "request body too large") {
			writeAPIError(writer, http.StatusRequestEntityTooLarge, errors.New("Uploads must total 100MB or less"))
		} else {
			writeAPIError(writer, http.StatusBadRequest, err)
		}
		return nil, false
	}
	if request.MultipartForm != nil {
		defer request.MultipartForm.RemoveAll()
	}
	if request.MultipartForm == nil {
		writeAPIError(writer, http.StatusBadRequest, errors.New("multipart form is required"))
		return nil, false
	}
	headers := request.MultipartForm.File["files"]
	var total int64
	for _, header := range headers {
		if header.Size > maxUploadFileBytes {
			writeAPIError(writer, http.StatusRequestEntityTooLarge, errors.New("Each upload must be 25MB or smaller"))
			return nil, false
		}
		total += header.Size
	}
	if total > maxUploadTotalBytes {
		writeAPIError(writer, http.StatusRequestEntityTooLarge, errors.New("Uploads must total 100MB or less"))
		return nil, false
	}
	files := make([]application.UploadFile, 0, len(headers))
	for _, header := range headers {
		data, err := readMultipartFile(header)
		if err != nil {
			writeAPIError(writer, http.StatusBadRequest, err)
			return nil, false
		}
		files = append(files, application.UploadFile{Name: header.Filename, Data: data})
	}
	return files, true
}

func readMultipartFile(header *multipart.FileHeader) ([]byte, error) {
	file, err := header.Open()
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxUploadFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxUploadFileBytes {
		return nil, errors.New("Each upload must be 25MB or smaller")
	}
	return data, nil
}

func writeUploadError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, application.ErrResourceAccessDenied):
		writeAPIError(writer, http.StatusForbidden, errors.New("Access denied"))
	case errors.Is(err, os.ErrNotExist):
		writeAPIError(writer, http.StatusNotFound, errors.New("Upload directory not found"))
	case errors.Is(err, application.ErrPathNotDirectory):
		writeAPIError(writer, http.StatusBadRequest, errors.New("Upload target is not a directory"))
	case errors.Is(err, application.ErrInvalidFileName):
		message := strings.TrimPrefix(err.Error(), application.ErrInvalidFileName.Error()+": ")
		writeAPIError(writer, http.StatusBadRequest, errors.New(message))
	default:
		writeAPIError(writer, http.StatusInternalServerError, err)
	}
}

func writeFileError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, application.ErrResourceAccessDenied):
		writeAPIError(writer, http.StatusForbidden, errors.New("Access denied"))
	case errors.Is(err, os.ErrNotExist):
		writeAPIError(writer, http.StatusNotFound, errors.New("Not found"))
	case errors.Is(err, application.ErrPathNotDirectory):
		writeAPIError(writer, http.StatusBadRequest, errors.New("Not a directory"))
	case errors.Is(err, application.ErrNotFile):
		writeAPIError(writer, http.StatusBadRequest, errors.New("Not a file"))
	default:
		writeAPIError(writer, http.StatusInternalServerError, err)
	}
}

func filePathFromWildcard(value string) string {
	value = strings.ReplaceAll(value, "\\", "/")
	if filepath.IsAbs(filepath.FromSlash(value)) || windowsAbsolutePath.MatchString(value) {
		return filepath.FromSlash(value)
	}
	return filepath.FromSlash("/" + strings.TrimLeft(value, "/"))
}

func fileExtension(path string) string {
	return surfacewire.FileExtension(path)
}

func fileLanguage(path string) string {
	return surfacewire.FileLanguage(path)
}

func imageMIME(path string) string    { return surfacewire.ImageMIME(path) }
func audioMIME(path string) string    { return surfacewire.AudioMIME(path) }
func documentMIME(path string) string { return surfacewire.DocumentMIME(path) }

func fileMIME(path string) string {
	return surfacewire.FileMIME(path)
}

func contentDisposition(disposition, fileName string) string {
	var fallback strings.Builder
	for _, character := range fileName {
		if character < 0x20 || character > 0x7e || strings.ContainsRune("\"\\;\r\n", character) {
			fallback.WriteByte('_')
		} else {
			fallback.WriteRune(character)
		}
	}
	if fallback.Len() == 0 {
		fallback.WriteString("download")
	}
	encoded := strings.ReplaceAll(url.QueryEscape(fileName), "+", "%20")
	return fmt.Sprintf(`%s; filename="%s"; filename*=UTF-8''%s`, disposition, fallback.String(), encoded)
}

func isoTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}
