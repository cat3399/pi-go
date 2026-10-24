package main

import (
	"io"
	"strings"
	"testing"
)

func TestReadSSECursorPreservesEpochAndDropsInterruptedFrame(t *testing.T) {
	input := strings.NewReader("id: epochOne:99\ndata: one\n\nid: epochTwo:1\ndata: reset\n\nid: epochTwo:2\ndata: interrupted")
	var ids, values []string
	err := readSSECursor(input, func(id, data string) error { ids = append(ids, id); values = append(values, data); return nil })
	if err != io.EOF || len(ids) != 2 || ids[0] != "epochOne:99" || ids[1] != "epochTwo:1" || values[1] != "reset" {
		t.Fatalf("SSE resume positions = %v %v %v", ids, values, err)
	}
	stream := &remoteEventStream{readCursor: ids[0]}
	stream.setOpaqueCursor(ids[1])
	if stream.opaqueCursor() != "epochTwo:1" {
		t.Fatal("cursor cannot follow a new stream with a lower sequence")
	}
	url, err := remoteReadEventsURL("https://example.com/prefix", stream.opaqueCursor(), `["selected"]`)
	if err != nil || url.Path != "/prefix/api/v1/events" || url.Query().Get("cursor") != "epochTwo:1" || url.Query().Get("sessions") != `["selected"]` {
		t.Fatalf("reconnect URL = %v %v", url, err)
	}
}
