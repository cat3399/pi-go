package web

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/cat3399/pi-go/internal/application"
)

func handleSessionImage(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		index, err := strconv.Atoi(request.URL.Query().Get("blockIndex"))
		if err != nil || index < 0 {
			writeAPIError(writer, http.StatusBadRequest, errors.New("valid blockIndex is required"))
			return
		}
		image, err := api.SessionImage(request.Context(), request.PathValue("id"), request.PathValue("entryId"), index)
		if err != nil {
			if errors.Is(err, application.ErrImageNotFound) || errors.Is(err, application.ErrSessionEntryNotFound) {
				writeAPIError(writer, http.StatusNotFound, err)
			} else {
				writeApplicationError(writer, err)
			}
			return
		}
		writer.Header().Set("Content-Type", image.MIMEType)
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
		writer.Header().Set("Cache-Control", "private, no-cache")
		writer.Header().Set("ETag", fmt.Sprintf(`"%x"`, sha256.Sum256(image.Data)))
		http.ServeContent(writer, request, "", time.Time{}, bytes.NewReader(image.Data))
	}
}
