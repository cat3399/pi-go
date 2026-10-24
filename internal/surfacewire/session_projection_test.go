package surfacewire

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestHistoryProjectionCombinesToolsThinkingAndMedia(t *testing.T) {
	raw := json.RawMessage(`{"role":"assistant","extra":{"n":9007199254740993},"content":[{"type":"text","text":"kept"},{"type":"thinking","thinking":"private","signature":"opaque"},{"type":"toolCall","id":"call-1","name":"read","arguments":{"offset":9007199254740993}},{"type":"image","data":"YWJjZA==","mimeType":"image/png"}]}`)
	want := json.RawMessage(`{"role":"assistant","extra":{"n":9007199254740993},"content":[{"type":"text","text":"kept"},{"type":"thinking","thinking":"","deferred":true,"signature":"opaque"},{"type":"toolCall","toolCallId":"call-1","toolName":"read","input":{"offset":9007199254740993}},{"type":"image","mimeType":"image/png","byteSize":4,"imageRef":{"entryId":"entry-1","blockIndex":3}}]}`)
	got, err := projectHistoryMessage(raw, true, true, true, "entry-1")
	if err != nil {
		t.Fatal(err)
	}
	decode := func(raw []byte) any {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	if !reflect.DeepEqual(decode(got), decode(want)) {
		t.Fatalf("projection = %s", got)
	}
}

func TestUnchangedHistoryProjectionRetainsEncodingAndOwnsBytes(t *testing.T) {
	for _, role := range []string{"user", "assistant", "toolResult"} {
		raw := json.RawMessage(" {\"role\":\"" + role + "\",\"content\":[{\"type\":\"text\",\"text\":\"hello\"}],\"extra\":1e3} ")
		got, err := projectHistoryMessage(raw, true, true, true, "entry-1")
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, raw) {
			t.Fatalf("unmodified %s encoding changed: %s", role, got)
		}
		got[0] = '!'
		if raw[0] != ' ' {
			t.Fatal("returned bytes alias input")
		}
	}
}

// Keep unchanged text and changed rich blocks in the same benchmark: a fast
// history path must not hide a regression when large media needs projection.
func BenchmarkHistoryProjection(b *testing.B) {
	for _, shape := range []struct {
		name, format string
		bytes        int
	}{
		{"text10KiB", `{"role":"assistant","content":[{"type":"text","text":"%s"}]}`, 10 * 1024},
		{"text1MiB", `{"role":"assistant","content":[{"type":"text","text":"%s"}]}`, 1024 * 1024},
		{"tool10KiB", `{"role":"assistant","content":[{"type":"toolCall","id":"call","name":"read","arguments":{"text":"%s"}}]}`, 10 * 1024},
		{"thinking1MiB", `{"role":"assistant","content":[{"type":"thinking","thinking":"%s"}]}`, 1024 * 1024},
		{"image1MiB", `{"role":"user","content":[{"type":"image","mimeType":"image/png","data":"%s"}]}`, 1024 * 1024},
	} {
		b.Run(shape.name, func(b *testing.B) {
			raw := json.RawMessage(strings.Replace(shape.format, "%s", strings.Repeat("A", shape.bytes), 1))
			var wireBytes int
			b.SetBytes(int64(len(raw)))
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				projected, err := projectHistoryMessage(raw, true, true, true, "entry")
				if err != nil {
					b.Fatal(err)
				}
				wireBytes = len(projected)
			}
			b.StopTimer()
			b.ReportMetric(float64(wireBytes), "wire-B/op")
		})
	}
}
