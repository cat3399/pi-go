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

// OpenAIResponsesReasoning is the deliberately small replay envelope needed
// for store:false Responses continuations. It does not carry an SDK item or
// arbitrary raw JSON. EncryptedContent is opaque and must never be logged.
type OpenAIResponsesReasoning struct {
	ItemID           string
	EncryptedContent string
	Redacted         bool
}

// OpenAIResponsesResponse is the response-level portion of the replay
// envelope. It deliberately excludes headers, body fragments, and SDK state.
type OpenAIResponsesResponse struct{ ResponseID, RawStopReason string }

func (r OpenAIResponsesResponse) validate() error {
	if !utf8.ValidString(r.ResponseID) || !utf8.ValidString(r.RawStopReason) || len(r.ResponseID) > 256 || len(r.RawStopReason) > 128 {
		return fmt.Errorf("%w: response replay metadata", ErrInvalidRichContent)
	}
	return nil
}

func (r OpenAIResponsesReasoning) validate() error {
	if !utf8.ValidString(r.ItemID) || strings.TrimSpace(r.ItemID) == "" || len(r.ItemID) > 256 {
		return fmt.Errorf("%w: reasoning item id", ErrInvalidRichContent)
	}
	if !utf8.ValidString(r.EncryptedContent) || len(r.EncryptedContent) > 1<<20 {
		return fmt.Errorf("%w: encrypted reasoning content", ErrInvalidRichContent)
	}
	return nil
}

// ThinkingBlock preserves readable reasoning plus the optional opaque OpenAI
// replay handle. Empty text is legal only when a replay handle exists.
type ThinkingBlock struct {
	thinking string
	replay   *OpenAIResponsesReasoning
}

func (ThinkingBlock) assistantBlock()          {}
func (ThinkingBlock) Kind() AssistantBlockKind { return AssistantBlockThinking }
func NewThinkingBlock(thinking string, replay *OpenAIResponsesReasoning) (ThinkingBlock, error) {
	b := ThinkingBlock{thinking: thinking}
	if replay != nil {
		c := *replay
		b.replay = &c
	}
	if err := b.validate(); err != nil {
		return ThinkingBlock{}, err
	}
	return b, nil
}
func (b ThinkingBlock) validate() error {
	if !utf8.ValidString(b.thinking) {
		return fmt.Errorf("%w: thinking is not UTF-8", ErrInvalidRichContent)
	}
	if b.replay != nil {
		if err := b.replay.validate(); err != nil {
			return err
		}
	}
	if b.thinking == "" && b.replay == nil {
		return fmt.Errorf("%w: empty thinking without replay handle", ErrInvalidRichContent)
	}
	return nil
}
func (b ThinkingBlock) Thinking() string { return b.thinking }
func (b ThinkingBlock) OpenAIResponsesReplay() (OpenAIResponsesReasoning, bool) {
	if b.replay == nil {
		return OpenAIResponsesReasoning{}, false
	}
	return *b.replay, true
}

// TextReplay is the typed message identity emitted by Responses. Phase is
// constrained so a stored text block cannot smuggle arbitrary vendor fields.
type TextReplay struct{ MessageID, Phase string }

func (r TextReplay) validate() error {
	if !utf8.ValidString(r.MessageID) || strings.TrimSpace(r.MessageID) == "" || len(r.MessageID) > 256 {
		return fmt.Errorf("%w: text message id", ErrInvalidRichContent)
	}
	if r.Phase != "" && r.Phase != "commentary" && r.Phase != "final_answer" {
		return fmt.Errorf("%w: text message phase", ErrInvalidRichContent)
	}
	return nil
}

// NewTextBlockWithReplay retains the response message identity needed for a
// stateless replay. NewTextBlock remains the normal no-metadata constructor.
func NewTextBlockWithReplay(text string, replay *TextReplay) (TextBlock, error) {
	b := TextBlock{text: text}
	if replay != nil {
		c := *replay
		b.replay = &c
	}
	if err := b.validate(); err != nil {
		return TextBlock{}, err
	}
	return b, nil
}
func (b TextBlock) TextReplay() (TextReplay, bool) {
	if b.replay == nil {
		return TextReplay{}, false
	}
	return *b.replay, true
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
