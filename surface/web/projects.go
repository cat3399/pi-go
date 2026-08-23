package web

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/surfacewire"
)

type projectPathRequest struct {
	Path string `json:"path"`
}

func decodeProjectPath(writer http.ResponseWriter, request *http.Request) (string, bool) {
	body, err := readRequestBody(writer, request)
	if err != nil {
		writeRequestBodyError(writer, err)
		return "", false
	}
	var input projectPathRequest
	if err := json.Unmarshal(body, &input); err != nil {
		writeAPIError(writer, http.StatusBadRequest, errors.New("path must be a string"))
		return "", false
	}
	return input.Path, true
}

func handleProjectAdd(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		path, ok := decodeProjectPath(writer, request)
		if !ok {
			return
		}
		project, err := surfacewire.AddProject(request.Context(), api, path)
		if err != nil {
			if errors.Is(err, surfacewire.ErrInvalidRequest) {
				writeAPIError(writer, http.StatusBadRequest, err)
				return
			}
			writeApplicationError(writer, err)
			return
		}
		writeJSON(writer, http.StatusCreated, map[string]any{"project": project})
	}
}

func handleProjectRemove(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		path, ok := decodeProjectPath(writer, request)
		if !ok {
			return
		}
		if err := surfacewire.RemoveProject(request.Context(), api, path); err != nil {
			if errors.Is(err, surfacewire.ErrInvalidRequest) {
				writeAPIError(writer, http.StatusBadRequest, err)
				return
			}
			writeApplicationError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"ok": true})
	}
}
