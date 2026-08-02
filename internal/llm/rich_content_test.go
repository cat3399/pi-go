package llm_test

import (
	"errors"
	"testing"
	"time"

	"github.com/cat3399/pi-go/internal/llm"
)

func TestRichContentCopiesAndValidatesBoundaries(t *testing.T) {
	data := []byte{1, 2, 3}
	image, err := llm.NewImageDataBlock("image/png", data)
	if err != nil {
		t.Fatal(err)
	}
	data[0] = 9
	copy := image.Data()
	copy[1] = 9
	if got := image.Data(); got[0] != 1 || got[1] != 2 {
		t.Fatalf("image alias = %v", got)
	}
	if _, err := llm.NewImageDataBlock("image/svg+xml", []byte{1}); !errors.Is(err, llm.ErrInvalidRichContent) {
		t.Fatalf("media error=%v", err)
	}
	if _, err := llm.NewImageURLBlock("image/png", "file:///secret"); !errors.Is(err, llm.ErrInvalidRichContent) {
		t.Fatalf("url error=%v", err)
	}
	if _, err := llm.NewThinkingBlock(""); !errors.Is(err, llm.ErrInvalidRichContent) {
		t.Fatalf("empty thinking=%v", err)
	}
	thinking, err := llm.NewThinkingBlockWithSignature("", "opaque-adapter-signature", true)
	if err != nil {
		t.Fatal(err)
	}
	message, err := llm.NewAssistantRichMessage([]llm.AssistantBlock{thinking}, llm.FinishStop, llm.Usage{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if err := llm.ValidateAssistantTerminal(message); err != nil {
		t.Fatal(err)
	}
	if signature, ok := thinking.ThinkingSignature(); !ok || signature != "opaque-adapter-signature" || !thinking.Redacted() {
		t.Fatalf("thinking metadata = (%q, %t, redacted=%t)", signature, ok, thinking.Redacted())
	}
	text, err := llm.NewTextBlockWithSignature("answer", `{"future":"provider-owned"}`)
	if err != nil {
		t.Fatal(err)
	}
	if signature, ok := text.TextSignature(); !ok || signature != `{"future":"provider-owned"}` {
		t.Fatalf("text signature = (%q, %t)", signature, ok)
	}
	call, err := llm.NewToolCallBlockWithThoughtSignature("call-1", "echo", []byte(`{}`), "opaque-tool-signature")
	if err != nil {
		t.Fatal(err)
	}
	if signature, ok := call.ThoughtSignature(); !ok || signature != "opaque-tool-signature" {
		t.Fatalf("thought signature = (%q, %t)", signature, ok)
	}
}

func FuzzRichContentAdmissionNeverAliasesOrPanics(f *testing.F) {
	f.Add("image/png", []byte{1, 2, 3}, "rs_1", "cipher")
	f.Add("image/svg+xml", []byte{}, "", "")
	f.Fuzz(func(t *testing.T, media string, data []byte, id, encrypted string) {
		image, err := llm.NewImageDataBlock(media, data)
		if err == nil {
			copy := image.Data()
			if len(copy) > 0 {
				copy[0] ^= 0xff
			}
			_ = image.Data()
		}
		_, _ = llm.NewThinkingBlockWithSignature("", id+encrypted, true)
	})
}
