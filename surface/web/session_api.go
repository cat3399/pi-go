package web

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/cat3399/pi-go/internal/application"
)

func handleSessionDelete(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		if err := api.DeleteSession(request.Context(), request.PathValue("id")); err != nil {
			writeApplicationError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"ok": true})
	}
}

func handleSessionExport(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		exported, err := api.ExportSession(request.Context(), request.PathValue("id"))
		if err != nil {
			writeApplicationError(writer, err)
			return
		}
		disposition := "attachment"
		if request.URL.Query().Get("inline") == "1" {
			disposition = "inline"
		}
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		writer.Header().Set("Content-Disposition", fmt.Sprintf(`%s; filename=%q`, disposition, exported.FileName))
		writer.Header().Set("Cache-Control", "no-cache")
		writer.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("X-Frame-Options", "DENY")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(exported.HTML)
	}
}

func handleSessionAutoName(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		if !hasJSONContentType(request) {
			writeAPIError(writer, http.StatusUnsupportedMediaType, errJSONContentType)
			return
		}
		generated, err := api.GenerateSessionTitle(request.Context(), request.PathValue("id"))
		if err != nil {
			writeApplicationError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{
			"title": generated.Title,
			"usage": map[string]uint64{
				"input": generated.Usage.Input, "output": generated.Usage.Output,
				"cacheRead": generated.Usage.CacheRead, "cacheWrite": generated.Usage.CacheWrite,
				"total": generated.Usage.Total,
			},
		})
	}
}

func handleSessionThinking(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		rawIndex := request.URL.Query().Get("blockIndex")
		blockIndex, err := strconv.Atoi(rawIndex)
		if err != nil || blockIndex < 0 {
			writeAPIError(writer, http.StatusBadRequest, errors.New("valid blockIndex is required"))
			return
		}
		thinking, err := api.SessionThinking(
			request.Context(), request.PathValue("id"), request.PathValue("entryId"), blockIndex,
		)
		if err != nil {
			switch {
			case errors.Is(err, application.ErrSessionEntryNotFound):
				writeAPIError(writer, http.StatusNotFound, errors.New("assistant message not found"))
			case errors.Is(err, application.ErrThinkingNotFound):
				writeAPIError(writer, http.StatusNotFound, errors.New("thinking block not found"))
			default:
				writeApplicationError(writer, err)
			}
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"thinking": thinking})
	}
}

func handleSessionBashOutput(api application.API) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		path := strings.TrimSpace(request.URL.Query().Get("path"))
		if path == "" {
			writeAPIError(writer, http.StatusBadRequest, errors.New("path is required"))
			return
		}
		output, err := api.OpenBashOutput(request.Context(), request.PathValue("id"), path)
		if err != nil {
			switch {
			case errors.Is(err, application.ErrInvalidBashOutput):
				writeAPIError(writer, http.StatusBadRequest, errors.New("invalid path"))
			case errors.Is(err, application.ErrBashOutputForbidden):
				writeAPIError(writer, http.StatusForbidden, errors.New("forbidden"))
			case errors.Is(err, os.ErrNotExist):
				writeAPIError(writer, http.StatusNotFound, errors.New("full output unavailable"))
			default:
				writeApplicationError(writer, err)
			}
			return
		}
		defer output.Reader.Close()
		if request.URL.Query().Get("download") == "1" {
			writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
			writer.Header().Set("Content-Disposition", `attachment; filename="bash-output.log"`)
			writer.Header().Set("Cache-Control", "no-store")
			writer.WriteHeader(http.StatusOK)
			_, _ = io.Copy(writer, output.Reader)
			return
		}
		if output.Size > application.MaxInlineBashOutputBytes {
			writeJSON(writer, http.StatusRequestEntityTooLarge, map[string]any{
				"error": fmt.Sprintf("full output is too large to display (limit %d bytes)", application.MaxInlineBashOutputBytes),
				"data":  map[string]int64{"size": output.Size, "maxBytes": application.MaxInlineBashOutputBytes},
			})
			return
		}
		content, err := io.ReadAll(io.LimitReader(output.Reader, application.MaxInlineBashOutputBytes+1))
		if err != nil {
			writeAPIError(writer, http.StatusNotFound, errors.New("full output unavailable"))
			return
		}
		if int64(len(content)) > application.MaxInlineBashOutputBytes {
			writeAPIError(writer, http.StatusRequestEntityTooLarge, application.ErrBashOutputTooLarge)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"output": string(content)}})
	}
}
