package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/surfacewire"
)

func handleReadEvents(api application.API, writer http.ResponseWriter, request *http.Request) {
	flusher, ok := writer.(http.Flusher)
	if !ok {
		writeAPIError(writer, http.StatusInternalServerError, errors.New("streaming is unavailable"))
		return
	}
	value := request.URL.Query().Get("cursor")
	if last := strings.TrimSpace(request.Header.Get("Last-Event-ID")); last != "" {
		value = last
	}
	cursor, err := surfacewire.DecodeEventCursor(value)
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, err)
		return
	}
	options := application.ReadEventOptions{After: cursor}
	if request.URL.Query().Has("sessions") {
		var selected []string
		if json.Unmarshal([]byte(request.URL.Query().Get("sessions")), &selected) != nil {
			writeAPIError(writer, http.StatusBadRequest, surfacewire.ErrInvalidRequest)
			return
		}
		options.Sessions = &selected
	}
	subscription, err := api.SubscribeReadEvents(options)
	reset := errors.Is(err, application.ErrEventCursorUnavailable)
	if reset {
		options.After = nil
		subscription, err = api.SubscribeReadEvents(options)
	}
	if err != nil {
		writeApplicationError(writer, err)
		return
	}
	serveApplicationEvents(writer, request, flusher, subscription, reset, true)
}

func writeStreamControl(writer io.Writer, subscription *application.EventSubscription, kind string, versioned bool) error {
	value := map[string]any{"type": kind, "revision": subscription.Revision}
	if !versioned {
		return writeSSE(writer, subscription.Revision, value)
	}
	cursor := surfacewire.EncodeEventCursor(subscription.Cursor)
	value["cursor"] = cursor
	return writeReadSSE(writer, cursor, value)
}

func writeStreamEvent(writer io.Writer, event application.Event, versioned bool) error {
	if !versioned {
		return writeApplicationSSE(writer, event)
	}
	cursor := surfacewire.EncodeEventCursor(application.EventCursor{Epoch: event.Epoch, Sequence: event.Sequence})
	envelope, err := surfacewire.EncodeEvent(event)
	if err != nil {
		envelope = surfacewire.EventEnvelope{Sequence: event.Sequence, Cursor: cursor, SessionID: event.SessionID, Event: map[string]any{"type": "protocol_error", "errorMessage": err.Error()}}
	}
	return writeReadSSE(writer, cursor, envelope)
}

func writeReadSSE(writer io.Writer, cursor string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(writer, "id: %s\ndata: %s\n\n", cursor, encoded)
	return err
}
