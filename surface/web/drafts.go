package web

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/surfacewire"
)

func handleDraftTextRead(api application.API) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		result, err := surfacewire.ReadDraftText(r.Context(), api, r.URL.Query().Get("key"))
		if err != nil {
			writeDraftError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, result)
	}
}

func handleDraftTextWrite(api application.API) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := readRequestBody(w, r)
		if err != nil {
			writeRequestBodyError(w, err)
			return
		}
		var input surfacewire.DraftTextUpdate
		if err := json.Unmarshal(body, &input); err != nil {
			writeAPIError(w, http.StatusBadRequest, err)
			return
		}
		result, err := surfacewire.WriteDraftText(r.Context(), api, r.URL.Query().Get("key"), input)
		if err != nil {
			writeDraftError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

func writeDraftError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, application.ErrInvalidDraft) {
		status = http.StatusBadRequest
	}
	writeAPIError(w, status, err)
}
