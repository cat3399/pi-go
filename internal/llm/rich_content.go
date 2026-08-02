package llm

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrInvalidRichContent identifies a malformed rich-content value. Rich
// content is deliberately a value type: callers never retain mutable image
// bytes or an untyped vendor payload owned by this package.
var ErrInvalidRichContent = errors.New("invalid rich content")

const maxImageBytes = 20 << 20

// ImageSource describes the two portable image forms accepted by conversation
// input. Data is preferred for durable sessions; URL is retained for callers
// that intentionally keep the remote-resource trust boundary outside pi-go.
type ImageSource uint8

const (
	ImageSourceData ImageSource = iota + 1
	ImageSourceURL
)

// ImageBlock is immutable image input. It is valid in user and tool-result
// content, not assistant output. The exact bytes returned by Data are cloned.
type ImageBlock struct {
	mediaType string
	source    ImageSource
	data      []byte
	url       string
}

func NewImageDataBlock(mediaType string, data []byte) (ImageBlock, error) {
	b := ImageBlock{mediaType: mediaType, source: ImageSourceData, data: bytes.Clone(data)}
	if err := b.validate(); err != nil {
		return ImageBlock{}, err
	}
	return b, nil
}

func NewImageURLBlock(mediaType, rawURL string) (ImageBlock, error) {
	b := ImageBlock{mediaType: mediaType, source: ImageSourceURL, url: rawURL}
	if err := b.validate(); err != nil {
		return ImageBlock{}, err
	}
	return b, nil
}

func (b ImageBlock) validate() error {
	if !utf8.ValidString(b.mediaType) {
		return fmt.Errorf("%w: media type is not UTF-8", ErrInvalidRichContent)
	}
	switch strings.ToLower(b.mediaType) {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return fmt.Errorf("%w: unsupported image media type %q", ErrInvalidRichContent, b.mediaType)
	}
	switch b.source {
	case ImageSourceData:
		if len(b.data) == 0 || len(b.data) > maxImageBytes {
			return fmt.Errorf("%w: image data size", ErrInvalidRichContent)
		}
	case ImageSourceURL:
		if !utf8.ValidString(b.url) || len(b.url) == 0 || len(b.url) > 8192 {
			return fmt.Errorf("%w: image URL", ErrInvalidRichContent)
		}
		u, err := url.Parse(b.url)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
			return fmt.Errorf("%w: image URL must be absolute HTTP(S)", ErrInvalidRichContent)
		}
	default:
		return fmt.Errorf("%w: unknown image source", ErrInvalidRichContent)
	}
	return nil
}
func (b ImageBlock) MediaType() string   { return b.mediaType }
func (b ImageBlock) Source() ImageSource { return b.source }
func (b ImageBlock) Data() []byte        { return bytes.Clone(b.data) }
func (b ImageBlock) URL() string         { return b.url }

// AssistantResponseMetadata contains optional provider-neutral identifiers
// exposed by an upstream response. Adapters decide whether any field is
// meaningful for replay; Agent and session storage only preserve it.
type AssistantResponseMetadata struct {
	ResponseID    string
	ResponseModel string
	RawStopReason string
}

func (r AssistantResponseMetadata) validate() error {
	for _, field := range []struct {
		value string
		limit int
	}{
		{value: r.ResponseID, limit: 256},
		{value: r.ResponseModel, limit: 512},
		{value: r.RawStopReason, limit: 128},
	} {
		if !utf8.ValidString(field.value) || len(field.value) > field.limit {
			return fmt.Errorf("%w: assistant response metadata", ErrInvalidRichContent)
		}
	}
	return nil
}

// AssistantProvenance identifies the provider dialect and model that produced
// replay metadata. Opaque IDs are usable only when this exactly matches the
// next request target.
type AssistantProvenance struct{ Provider, API, Model string }

func (p AssistantProvenance) validate() error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "provider", value: p.Provider},
		{name: "api", value: p.API},
		{name: "model", value: p.Model},
	} {
		if !utf8.ValidString(field.value) || strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%w: assistant provenance %s", ErrInvalidRichContent, field.name)
		}
	}
	return nil
}

func (p AssistantProvenance) Matches(provider, api, model string) bool {
	return p.Provider == provider && p.API == api && p.Model == model
}

// ThinkingBlock mirrors pi's provider-neutral thinking content. The signature
// is an opaque adapter-owned string; Agent and session storage preserve it but
// never branch on its wire shape. Empty text is legal only with a signature.
type ThinkingBlock struct {
	thinking          string
	thinkingSignature string
	redacted          bool
}

func NewThinkingBlockWithSignature(thinking, signature string, redacted bool) (ThinkingBlock, error) {
	b := ThinkingBlock{thinking: thinking, thinkingSignature: signature, redacted: redacted}
	if err := b.validate(); err != nil {
		return ThinkingBlock{}, err
	}
	return b, nil
}

func (ThinkingBlock) assistantBlock()          {}
func (ThinkingBlock) Kind() AssistantBlockKind { return AssistantBlockThinking }
func NewThinkingBlock(thinking string) (ThinkingBlock, error) {
	return NewThinkingBlockWithSignature(thinking, "", false)
}
func (b ThinkingBlock) validate() error {
	if !utf8.ValidString(b.thinking) {
		return fmt.Errorf("%w: thinking is not UTF-8", ErrInvalidRichContent)
	}
	if err := validateOpaqueSignature(b.thinkingSignature); err != nil {
		return err
	}
	if b.thinking == "" && b.thinkingSignature == "" {
		return fmt.Errorf("%w: empty thinking without replay handle", ErrInvalidRichContent)
	}
	if b.redacted && b.thinkingSignature == "" {
		return fmt.Errorf("%w: redacted thinking without signature", ErrInvalidRichContent)
	}
	return nil
}
func (b ThinkingBlock) Thinking() string { return b.thinking }
func (b ThinkingBlock) ThinkingSignature() (string, bool) {
	return b.thinkingSignature, b.thinkingSignature != ""
}
func (b ThinkingBlock) Redacted() bool { return b.redacted }

func validateOpaqueSignature(signature string) error {
	if !utf8.ValidString(signature) || len(signature) > 2<<20 {
		return fmt.Errorf("%w: invalid opaque content signature", ErrInvalidRichContent)
	}
	return nil
}

func NewTextBlockWithSignature(text, signature string) (TextBlock, error) {
	b := TextBlock{text: text, textSignature: signature}
	if err := b.validate(); err != nil {
		return TextBlock{}, err
	}
	return b, nil
}
func (b TextBlock) TextSignature() (string, bool) {
	return b.textSignature, b.textSignature != ""
}

// UserContentBlock and ToolResultContentBlock prevent an image from being
// accepted in assistant output by accident.
type UserContentBlock interface{ userContentBlock() }

func (TextBlock) userContentBlock()  {}
func (ImageBlock) userContentBlock() {}

type ToolResultContentBlock interface{ toolResultContentBlock() }

func (TextBlock) toolResultContentBlock()  {}
func (ImageBlock) toolResultContentBlock() {}

type UserContentMessage struct {
	content   []UserContentBlock
	timestamp time.Time
}

func (UserContentMessage) conversationMessage() {}
func NewUserContentMessage(content []UserContentBlock, timestamp time.Time) (UserContentMessage, error) {
	m := UserContentMessage{content: append([]UserContentBlock(nil), content...), timestamp: timestamp}
	if err := m.validate(); err != nil {
		return UserContentMessage{}, err
	}
	return m, nil
}
func (m UserContentMessage) validate() error {
	for _, b := range m.content {
		switch b := b.(type) {
		case TextBlock:
			if err := b.validate(); err != nil {
				return err
			}
		case ImageBlock:
			if err := b.validate(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: user block %T", ErrInvalidRichContent, b)
		}
	}
	return nil
}
func (UserContentMessage) Role() Role             { return RoleUser }
func (m UserContentMessage) Timestamp() time.Time { return m.timestamp }
func (m UserContentMessage) Content() []UserContentBlock {
	return append([]UserContentBlock(nil), m.content...)
}
