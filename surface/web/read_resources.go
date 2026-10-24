package web

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/surfacewire"
)

func handleSessionResource(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		var query surfacewire.ReadResourceRequest
		if encoded := request.URL.Query().Get("query"); encoded != "" {
			if len(encoded) > 16384 || json.Unmarshal([]byte(encoded), &query) != nil {
				writeAPIError(writer, http.StatusBadRequest, surfacewire.ErrInvalidRequest)
				return
			}
		}
		known := request.URL.Query().Get("known")
		conditional := request.Header.Get("If-None-Match")
		value, err := surfacewire.ReadSessionResource(request.Context(), api, request.PathValue("id"), request.PathValue("resource"), query, known)
		if err != nil {
			writeApplicationError(writer, err)
			return
		}
		writer.Header().Set("ETag", `"`+value.Token+`"`)
		writer.Header().Set("Cache-Control", "private, no-cache")
		if matchesResourceETag(conditional, value.Token) {
			writer.WriteHeader(http.StatusNotModified)
			return
		}
		writeJSON(writer, http.StatusOK, value)
	}
}

func matchesResourceETag(header, token string) bool {
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "*" || strings.TrimPrefix(part, "W/") == `"`+token+`"` {
			return true
		}
	}
	return false
}
