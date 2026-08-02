package provider

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestResponsesSSEDecoderAcceptsAllLineEndingsAndDataLines(t *testing.T) {
	for _, separator := range []string{"\n", "\r\n", "\r"} {
		name := strings.NewReplacer("\r", "CR", "\n", "LF").Replace(separator)
		t.Run(name, func(t *testing.T) {
			body := strings.Join([]string{
				": comment",
				"event: ignored",
				"id: ignored",
				"retry: 10",
				"data: first",
				"unknown: ignored",
				"data:second",
				"",
				"",
			}, separator)
			decoder := newResponsesSSEDecoder(strings.NewReader(body), 1024)
			data, err := decoder.NextData()
			if err != nil {
				t.Fatalf("NextData() error = %v", err)
			}
			if want := []byte("first\nsecond"); !bytes.Equal(data, want) {
				t.Fatalf("data = %q, want %q", data, want)
			}
			if _, err := decoder.NextData(); !errors.Is(err, io.EOF) {
				t.Fatalf("second NextData() error = %v, want EOF", err)
			}
		})
	}
}

func TestResponsesSSEDecoderRejectsUnterminatedFinalFrame(t *testing.T) {
	for _, body := range []string{
		"data: lost",
		"data: lost\n",
		"data: lost\r",
		"data: lost\r\n",
	} {
		decoder := newResponsesSSEDecoder(strings.NewReader(body), 1024)
		if data, err := decoder.NextData(); !errors.Is(err, errResponsesIncompleteFrame) || data != nil {
			t.Fatalf("NextData(%q) = %q/%v, want nil/errResponsesIncompleteFrame", body, data, err)
		}
	}

	decoder := newResponsesSSEDecoder(strings.NewReader("data: kept\n\ndata: lost"), 1024)
	data, err := decoder.NextData()
	if err != nil || string(data) != "kept" {
		t.Fatalf("first NextData() = %q/%v", data, err)
	}
	if data, err := decoder.NextData(); !errors.Is(err, errResponsesIncompleteFrame) || data != nil {
		t.Fatalf("second NextData() = %q/%v, want nil/errResponsesIncompleteFrame", data, err)
	}
}

func TestResponsesSSEDecoderBoundsWholeFrame(t *testing.T) {
	decoder := newResponsesSSEDecoder(strings.NewReader("data: 1234\ndata: 5678\n\n"), 12)
	if _, err := decoder.NextData(); !errors.Is(err, errResponsesEventTooLarge) {
		t.Fatalf("NextData() error = %v, want errResponsesEventTooLarge", err)
	}

	decoder = newResponsesSSEDecoder(strings.NewReader("data:x\n\n"), len("data:x"))
	if data, err := decoder.NextData(); err != nil || string(data) != "x" {
		t.Fatalf("exact-limit NextData() = %q/%v", data, err)
	}
}

func FuzzResponsesSSEDecoderNeverPanics(f *testing.F) {
	for _, seed := range []string{
		"",
		"data: {}\n\n",
		"data: one\rdata: two\r\r",
		"data: unterminated",
		strings.Repeat("x", 300),
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		if len(input) > 4096 {
			t.Skip()
		}
		decoder := newResponsesSSEDecoder(bytes.NewReader(input), 256)
		for attempts := 0; attempts <= len(input)+1; attempts++ {
			_, err := decoder.NextData()
			if err != nil {
				return
			}
		}
		t.Fatal("decoder did not make progress")
	})
}
