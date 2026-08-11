package web

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/cat3399/pi-go/internal/application"
)

var windowsAbsolutePath = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

func handleFileIndex(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		cwd := strings.TrimSpace(request.URL.Query().Get("cwd"))
		if !isAbsoluteFilePath(cwd) {
			writeAPIError(writer, http.StatusBadRequest, errors.New("cwd must be an absolute path"))
			return
		}
		result, err := api.QueryFileIndex(request.Context(), cwd, request.URL.Query().Get("q"))
		if err != nil {
			writeFileResourceError(writer, err)
			return
		}
		if result.HasQuery {
			writeJSON(writer, http.StatusOK, map[string]any{"matches": result.Matches})
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"files": result.Files, "truncated": result.Truncated})
	}
}

func handleGitStatus(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		cwd := strings.TrimSpace(request.URL.Query().Get("cwd"))
		if !isAbsoluteFilePath(cwd) {
			writeAPIError(writer, http.StatusBadRequest, errors.New("cwd must be an absolute path"))
			return
		}
		result, err := api.GetGitStatus(request.Context(), cwd)
		if err != nil {
			writeFileResourceError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, result)
	}
}

func handleGitDiff(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		cwd := strings.TrimSpace(request.URL.Query().Get("cwd"))
		filePath := strings.TrimSpace(request.URL.Query().Get("path"))
		if !isAbsoluteFilePath(cwd) {
			writeAPIError(writer, http.StatusBadRequest, errors.New("cwd must be an absolute path"))
			return
		}
		if !isAbsoluteFilePath(filePath) {
			writeAPIError(writer, http.StatusBadRequest, errors.New("path must be an absolute path"))
			return
		}
		result, err := api.GetGitFileDiff(request.Context(), cwd, filePath)
		if err != nil {
			writeFileResourceError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, result)
	}
}

func isAbsoluteFilePath(value string) bool {
	return value != "" && (filepath.IsAbs(value) || windowsAbsolutePath.MatchString(value))
}

func writeFileResourceError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, application.ErrResourceAccessDenied):
		writeAPIError(writer, http.StatusForbidden, errors.New("Access denied"))
	case errors.Is(err, os.ErrNotExist):
		writeAPIError(writer, http.StatusNotFound, errors.New("Directory not found"))
	case errors.Is(err, application.ErrPathNotDirectory):
		writeAPIError(writer, http.StatusBadRequest, errors.New("Not a directory"))
	default:
		writeAPIError(writer, http.StatusInternalServerError, err)
	}
}
