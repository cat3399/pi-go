package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cat3399/pi-go/internal/application"
)

type readCursorTestAPI struct {
	application.API
	cursor application.EventCursor
	seen   []application.ReadEventOptions
}

func (a *readCursorTestAPI) SubscribeReadEvents(options application.ReadEventOptions) (*application.EventSubscription, error) {
	a.seen = append(a.seen, options)
	if options.After != nil && options.After.Epoch != a.cursor.Epoch {
		return nil, application.ErrEventCursorUnavailable
	}
	events := make(chan application.Event)
	close(events)
	return &application.EventSubscription{Cursor: a.cursor, Revision: a.cursor.Sequence, Events: events}, nil
}

func TestReadSSECursorDetectsSameSequenceRestartAndPrefersReconnectHeader(t *testing.T) {
	api := &readCursorTestAPI{cursor: application.EventCursor{Epoch: "newEpoch", Sequence: 7}}
	handler := handleApplicationEvents(api)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/events?cursor=oldEpoch:7&sessions=%5B%5D", nil)
	response := httptest.NewRecorder()
	handler(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "id: newEpoch:7\n") || !strings.Contains(response.Body.String(), `"type":"reset_required"`) || !strings.Contains(response.Body.String(), `"cursor":"newEpoch:7"`) {
		t.Fatalf("restart response = %d %s", response.Code, response.Body)
	}
	if len(api.seen) != 2 || api.seen[0].After.Epoch != "oldEpoch" || api.seen[1].After != nil || api.seen[1].Sessions == nil || len(*api.seen[1].Sessions) != 0 {
		t.Fatalf("scope/reset = %+v", api.seen)
	}
	api.seen = nil
	request = httptest.NewRequest(http.MethodGet, "/api/v1/events?cursor=oldEpoch:1&sessions=%5B%22selected%22%5D", nil)
	request.Header.Set("Last-Event-ID", "newEpoch:5")
	response = httptest.NewRecorder()
	handler(response, request)
	if len(api.seen) != 1 || api.seen[0].After.Epoch != "newEpoch" || api.seen[0].After.Sequence != 5 || (*api.seen[0].Sessions)[0] != "selected" || strings.Contains(response.Body.String(), "reset_required") {
		t.Fatalf("reconnect used original query: %+v %s", api.seen, response.Body)
	}
}
