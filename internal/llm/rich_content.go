package llm

import (
	"bytes"
	"encoding/json"
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

// AssistantDiagnostic is pi's redacted, provider-neutral diagnostic record.
// Error code and details remain raw JSON so session round-trips do not coerce
// large numbers or expose adapter-specific structs to the agent loop.
type AssistantDiagnostic struct {
	typ       string
	timestamp time.Time
	errorInfo *AssistantDiagnosticError
	details   json.RawMessage
}

type AssistantDiagnosticError struct {
	Name, Message, Stack string
	Code                 json.RawMessage
}

type AssistantDiagnosticSpec struct {
	Type      string
	Timestamp time.Time
	Error     *AssistantDiagnosticError
	Details   json.RawMessage
}

func NewAssistantDiagnostic(spec AssistantDiagnosticSpec) (AssistantDiagnostic, error) {
	diagnostic := AssistantDiagnostic{
		typ: spec.Type, timestamp: spec.Timestamp.UTC().Truncate(time.Millisecond),
		details: bytes.Clone(spec.Details),
	}
	if spec.Error != nil {
		copy := *spec.Error
		copy.Code = bytes.Clone(spec.Error.Code)
		diagnostic.errorInfo = &copy
	}
	if err := diagnostic.validate(); err != nil {
		return AssistantDiagnostic{}, err
	}
	return diagnostic, nil
}

func (d AssistantDiagnostic) validate() error {
	if !utf8.ValidString(d.typ) || strings.TrimSpace(d.typ) == "" || len(d.typ) > 256 {
		return fmt.Errorf("%w: assistant diagnostic type", ErrInvalidRichContent)
	}
	if d.timestamp.IsZero() || !time.UnixMilli(d.timestamp.UnixMilli()).Equal(d.timestamp) {
		return fmt.Errorf("%w: assistant diagnostic timestamp", ErrInvalidRichContent)
	}
	if d.errorInfo != nil {
		for _, value := range []string{d.errorInfo.Name, d.errorInfo.Message, d.errorInfo.Stack} {
			if !utf8.ValidString(value) {
				return fmt.Errorf("%w: assistant diagnostic error", ErrInvalidRichContent)
			}
		}
		if strings.TrimSpace(d.errorInfo.Message) == "" {
			return fmt.Errorf("%w: assistant diagnostic error message", ErrInvalidRichContent)
		}
		if len(d.errorInfo.Code) != 0 {
			var code any
			if json.Unmarshal(d.errorInfo.Code, &code) != nil {
				return fmt.Errorf("%w: assistant diagnostic error code", ErrInvalidRichContent)
			}
			switch code.(type) {
			case string, float64:
			default:
				return fmt.Errorf("%w: assistant diagnostic error code", ErrInvalidRichContent)
			}
		}
	}
	if len(d.details) != 0 {
		var object map[string]json.RawMessage
		if json.Unmarshal(d.details, &object) != nil || object == nil {
			return fmt.Errorf("%w: assistant diagnostic details", ErrInvalidRichContent)
		}
	}
	return nil
}

func (d AssistantDiagnostic) Type() string         { return d.typ }
func (d AssistantDiagnostic) Timestamp() time.Time { return d.timestamp }
func (d AssistantDiagnostic) ErrorInfo() (AssistantDiagnosticError, bool) {
	if d.errorInfo == nil {
		return AssistantDiagnosticError{}, false
	}
	copy := *d.errorInfo
	copy.Code = bytes.Clone(d.errorInfo.Code)
	return copy, true
}
func (d AssistantDiagnostic) Details() json.RawMessage { return bytes.Clone(d.details) }

// AssistantMetadata contains the fields shared by every terminal assistant
// variant. Provenance is required; response metadata and diagnostics follow
// pi's optional fields.
type AssistantMetadata struct {
	Provenance  AssistantProvenance
	Response    *AssistantResponseMetadata
	Diagnostics []AssistantDiagnostic
}

func (m AssistantMetadata) validate() error {
	if err := m.Provenance.validate(); err != nil {
		return err
	}
	if m.Response != nil {
		if err := m.Response.validate(); err != nil {
			return err
		}
	}
	for _, diagnostic := range m.Diagnostics {
		if err := diagnostic.validate(); err != nil {
			return err
		}
	}
	return nil
}

func cloneAssistantMetadata(metadata AssistantMetadata) AssistantMetadata {
	if metadata.Response != nil {
		copy := *metadata.Response
		metadata.Response = &copy
	}
	metadata.Diagnostics = cloneAssistantDiagnostics(metadata.Diagnostics)
	return metadata
}

func cloneAssistantDiagnostics(values []AssistantDiagnostic) []AssistantDiagnostic {
	if values == nil {
		return nil
	}
	result := make([]AssistantDiagnostic, len(values))
	for index, value := range values {
		result[index] = value
		result[index].details = bytes.Clone(value.details)
		if value.errorInfo != nil {
			copy := *value.errorInfo
			copy.Code = bytes.Clone(value.errorInfo.Code)
			result[index].errorInfo = &copy
		}
	}
	return result
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
