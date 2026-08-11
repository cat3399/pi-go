package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/cat3399/pi-go/internal/application"
)

var dirtyWorktreeError = regexp.MustCompile(`(?i)contains modified or untracked files|is dirty`)

func handleWorktreeList(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		cwd := strings.TrimSpace(request.URL.Query().Get("cwd"))
		if cwd == "" {
			writeAPIError(writer, http.StatusBadRequest, errors.New("cwd is required"))
			return
		}
		result, err := api.ListWorktrees(request.Context(), cwd)
		if err != nil {
			writeWorktreeError(writer, err, http.StatusInternalServerError)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{
			"projectRoot": result.ProjectRoot,
			"isGit":       result.IsGit,
			"isTopLevel":  result.IsTopLevel,
			"worktrees":   result.Worktrees,
		})
	}
}

func handleWorktreeAdd(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		body, err := readRequestBody(writer, request)
		if err != nil {
			writeRequestBodyError(writer, err)
			return
		}
		var input struct {
			CWD    string `json:"cwd"`
			Branch string `json:"branch"`
		}
		if err := json.Unmarshal(body, &input); err != nil {
			writeAPIError(writer, http.StatusBadRequest, errors.New("cwd and branch must be strings"))
			return
		}
		if strings.TrimSpace(input.CWD) == "" {
			writeAPIError(writer, http.StatusBadRequest, errors.New("cwd is required"))
			return
		}
		if strings.TrimSpace(input.Branch) == "" {
			writeAPIError(writer, http.StatusBadRequest, errors.New("branch is required"))
			return
		}
		result, err := api.AddWorktree(request.Context(), input.CWD, input.Branch)
		if err != nil {
			writeWorktreeError(writer, err, http.StatusBadRequest)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"path": result.Path, "branch": result.Branch})
	}
}

func handleWorktreeRemove(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		body, err := readRequestBody(writer, request)
		if err != nil {
			writeRequestBodyError(writer, err)
			return
		}
		var input struct {
			CWD   string `json:"cwd"`
			Path  string `json:"path"`
			Force bool   `json:"force"`
		}
		if err := json.Unmarshal(body, &input); err != nil {
			writeAPIError(writer, http.StatusBadRequest, errors.New("cwd and path must be strings"))
			return
		}
		if strings.TrimSpace(input.CWD) == "" {
			writeAPIError(writer, http.StatusBadRequest, errors.New("cwd is required"))
			return
		}
		if strings.TrimSpace(input.Path) == "" {
			writeAPIError(writer, http.StatusBadRequest, errors.New("path is required"))
			return
		}
		if err := api.RemoveWorktree(request.Context(), input.CWD, input.Path, input.Force); err != nil {
			if dirtyWorktreeError.MatchString(err.Error()) {
				writeJSON(writer, http.StatusConflict, map[string]any{"error": err.Error(), "dirty": true})
				return
			}
			writeWorktreeError(writer, err, http.StatusBadRequest)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"success": true})
	}
}

func writeWorktreeError(writer http.ResponseWriter, err error, fallbackStatus int) {
	switch {
	case errors.Is(err, application.ErrResourceAccessDenied):
		writeAPIError(writer, http.StatusForbidden, errors.New("Access denied"))
	case errors.Is(err, application.ErrPathNotDirectory):
		writeAPIError(writer, http.StatusBadRequest, err)
	case errors.Is(err, os.ErrNotExist):
		writeAPIError(writer, http.StatusBadRequest, err)
	default:
		writeAPIError(writer, fallbackStatus, err)
	}
}
