package acpagent

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"

	acp "github.com/coder/acp-go-sdk"
	"google.golang.org/genai"
)

// ErrPromptCapabilityUnsupported reports that a prompt contains an optional
// ACP content block which the initialized agent did not advertise.
var ErrPromptCapabilityUnsupported = errors.New("acp prompt capability unsupported")

// PromptCapabilityError describes a capability mismatch without including
// prompt bytes, text, URIs, or filesystem paths.
type PromptCapabilityError struct {
	PartKind           string
	MIMEClass          string
	RequiredCapability string
	Fallback           string
}

func (e *PromptCapabilityError) Error() string {
	if e == nil {
		return ErrPromptCapabilityUnsupported.Error()
	}

	message := ErrPromptCapabilityUnsupported.Error()
	if e.PartKind != "" {
		message += ": " + e.PartKind
	}
	if e.MIMEClass != "" {
		message += " (" + e.MIMEClass + ")"
	}
	if e.RequiredCapability != "" {
		message += " requires promptCapabilities." + e.RequiredCapability
	}
	if e.Fallback != "" {
		message += "; use " + e.Fallback + " instead"
	}
	return message
}

func (e *PromptCapabilityError) Unwrap() error {
	return ErrPromptCapabilityUnsupported
}

type promptSupport struct {
	image           bool
	audio           bool
	embeddedContext bool
}

func promptSupportFromCapabilities(capabilities acp.PromptCapabilities) promptSupport {
	return promptSupport{
		image:           capabilities.Image,
		audio:           capabilities.Audio,
		embeddedContext: capabilities.EmbeddedContext,
	}
}

func allPromptSupport() promptSupport {
	return promptSupport{image: true, audio: true, embeddedContext: true}
}

func promptContentBlocks(content *genai.Content, support promptSupport) ([]acp.ContentBlock, error) {
	if content == nil {
		return nil, fmt.Errorf("prompt content is empty")
	}

	blocks := make([]acp.ContentBlock, 0, len(content.Parts))
	for i, part := range content.Parts {
		block, include, err := promptContentBlock(part, support)
		if err != nil {
			return nil, fmt.Errorf("convert prompt part %d: %w", i, err)
		}
		if include {
			blocks = append(blocks, block)
		}
	}
	if len(blocks) == 0 {
		return nil, fmt.Errorf("prompt content is empty")
	}
	return blocks, nil
}

func promptContentBlock(part *genai.Part, support promptSupport) (acp.ContentBlock, bool, error) {
	if part == nil {
		return acp.ContentBlock{}, false, fmt.Errorf("part is nil")
	}

	kinds := promptPartKinds(part)
	if len(kinds) > 1 {
		return acp.ContentBlock{}, false, fmt.Errorf("part has multiple content fields: %s", strings.Join(kinds, ", "))
	}
	if len(kinds) == 0 {
		return acp.ContentBlock{}, false, fmt.Errorf("part has no supported content field")
	}

	switch kinds[0] {
	case "text":
		if strings.TrimSpace(part.Text) == "" {
			return acp.ContentBlock{}, false, nil
		}
		return acp.TextBlock(part.Text), true, nil
	case "inline_data":
		return inlineDataBlock(part.InlineData, support)
	case "file_data":
		return fileDataBlock(part.FileData)
	default:
		return acp.ContentBlock{}, false, fmt.Errorf("unsupported ADK content field %s", kinds[0])
	}
}

func promptPartKinds(part *genai.Part) []string {
	kinds := make([]string, 0, 3)
	if part.Text != "" {
		kinds = append(kinds, "text")
	}
	if part.InlineData != nil {
		kinds = append(kinds, "inline_data")
	}
	if part.FileData != nil {
		kinds = append(kinds, "file_data")
	}
	if part.FunctionCall != nil {
		kinds = append(kinds, "function_call")
	}
	if part.FunctionResponse != nil {
		kinds = append(kinds, "function_response")
	}
	if part.ExecutableCode != nil {
		kinds = append(kinds, "executable_code")
	}
	if part.CodeExecutionResult != nil {
		kinds = append(kinds, "code_execution_result")
	}
	if part.ToolCall != nil {
		kinds = append(kinds, "tool_call")
	}
	if part.ToolResponse != nil {
		kinds = append(kinds, "tool_response")
	}
	return kinds
}

func inlineDataBlock(blob *genai.Blob, support promptSupport) (acp.ContentBlock, bool, error) {
	if blob == nil || len(blob.Data) == 0 {
		return acp.ContentBlock{}, false, fmt.Errorf("inline data is empty")
	}

	mimeType := strings.ToLower(strings.TrimSpace(blob.MIMEType))
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		if !support.image && !support.embeddedContext {
			return acp.ContentBlock{}, false, newPromptCapabilityError("inline_data", "image", "image", "embeddedContext")
		}
		data := base64.StdEncoding.EncodeToString(blob.Data)
		if support.image {
			return acp.ImageBlock(data, mimeType), true, nil
		}
		return embeddedResourceBlock(data, blob.Data, mimeType), true, nil
	case strings.HasPrefix(mimeType, "audio/"):
		if !support.audio && !support.embeddedContext {
			return acp.ContentBlock{}, false, newPromptCapabilityError("inline_data", "audio", "audio", "embeddedContext")
		}
		data := base64.StdEncoding.EncodeToString(blob.Data)
		if support.audio {
			return acp.AudioBlock(data, mimeType), true, nil
		}
		return embeddedResourceBlock(data, blob.Data, mimeType), true, nil
	default:
		if !support.embeddedContext {
			return acp.ContentBlock{}, false, newPromptCapabilityError("inline_data", mimeClass(mimeType), "embeddedContext", "provide FileData")
		}
		data := base64.StdEncoding.EncodeToString(blob.Data)
		return embeddedResourceBlock(data, blob.Data, mimeType), true, nil
	}
}

func embeddedResourceBlock(data string, raw []byte, mimeType string) acp.ContentBlock {
	sum := sha256.Sum256(raw)
	contents := &acp.BlobResourceContents{
		Blob: data,
		Uri:  fmt.Sprintf("urn:adk:inline:%x", sum),
	}
	if mimeType != "" {
		contents.MimeType = &mimeType
	}
	return acp.ResourceBlock(acp.EmbeddedResourceResource{
		BlobResourceContents: contents,
	})
}

func fileDataBlock(file *genai.FileData) (acp.ContentBlock, bool, error) {
	if file == nil {
		return acp.ContentBlock{}, false, fmt.Errorf("file data is empty")
	}
	uri := strings.TrimSpace(file.FileURI)
	if uri == "" {
		return acp.ContentBlock{}, false, fmt.Errorf("file URI is empty")
	}
	mimeType := strings.ToLower(strings.TrimSpace(file.MIMEType))

	name := strings.TrimSpace(file.DisplayName)
	if name == "" {
		name = resourceName(uri)
	}
	block := acp.ResourceLinkBlock(name, uri)
	if mimeType != "" {
		block.ResourceLink.MimeType = &mimeType
	}
	return block, true, nil
}

func newPromptCapabilityError(partKind, mimeType, required, fallback string) error {
	return &PromptCapabilityError{
		PartKind:           partKind,
		MIMEClass:          mimeType,
		RequiredCapability: required,
		Fallback:           fallback,
	}
}

func mimeClass(mimeType string) string {
	if mimeType == "" {
		return "untyped"
	}
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		return "image"
	case strings.HasPrefix(mimeType, "audio/"):
		return "audio"
	case strings.HasPrefix(mimeType, "text/"):
		return "text"
	case strings.HasPrefix(mimeType, "application/"):
		return "application"
	default:
		return "other"
	}
}

func validatePromptBlocks(blocks []acp.ContentBlock, support promptSupport) error {
	for i, block := range blocks {
		switch {
		case block.Text != nil, block.ResourceLink != nil:
			continue
		case block.Image != nil:
			if !support.image {
				return fmt.Errorf("validate prompt block %d: %w", i, newPromptCapabilityError("image", "image", "image", "ResourceLink or embeddedContext"))
			}
		case block.Audio != nil:
			if !support.audio {
				return fmt.Errorf("validate prompt block %d: %w", i, newPromptCapabilityError("audio", "audio", "audio", "ResourceLink or embeddedContext"))
			}
		case block.Resource != nil:
			if !support.embeddedContext {
				return fmt.Errorf("validate prompt block %d: %w", i, newPromptCapabilityError("resource", "embedded", "embeddedContext", "ResourceLink"))
			}
		default:
			return fmt.Errorf("validate prompt block %d: content block is empty", i)
		}
	}
	return nil
}

func resourceName(uri string) string {
	parsed, err := url.Parse(uri)
	if err == nil {
		if name := path.Base(strings.TrimSpace(parsed.Path)); name != "" && name != "." && name != "/" {
			return name
		}
	}
	return "resource"
}

func prependInstructionsToContent(instructions string, prompt []acp.ContentBlock) []acp.ContentBlock {
	instructions = strings.TrimSpace(instructions)
	if instructions == "" {
		return append([]acp.ContentBlock(nil), prompt...)
	}

	blocks := make([]acp.ContentBlock, 0, len(prompt)+1)
	blocks = append(blocks, acp.TextBlock(instructions+"\n\nUser message:\n"))
	blocks = append(blocks, prompt...)
	return blocks
}

type promptBlockLog struct {
	Type  string `json:"type"`
	Bytes int    `json:"bytes"`
}

func promptBlockLogs(blocks []acp.ContentBlock) []promptBlockLog {
	out := make([]promptBlockLog, 0, len(blocks))
	for _, block := range blocks {
		switch {
		case block.Text != nil:
			out = append(out, promptBlockLog{Type: "text", Bytes: len(block.Text.Text)})
		case block.Image != nil:
			out = append(out, promptBlockLog{Type: "image", Bytes: base64DecodedLen(block.Image.Data)})
		case block.Audio != nil:
			out = append(out, promptBlockLog{Type: "audio", Bytes: base64DecodedLen(block.Audio.Data)})
		case block.ResourceLink != nil:
			out = append(out, promptBlockLog{Type: "resource_link"})
		case block.Resource != nil && block.Resource.Resource.BlobResourceContents != nil:
			out = append(out, promptBlockLog{
				Type:  "resource",
				Bytes: base64DecodedLen(block.Resource.Resource.BlobResourceContents.Blob),
			})
		case block.Resource != nil && block.Resource.Resource.TextResourceContents != nil:
			out = append(out, promptBlockLog{
				Type:  "resource",
				Bytes: len(block.Resource.Resource.TextResourceContents.Text),
			})
		default:
			out = append(out, promptBlockLog{Type: "unknown"})
		}
	}
	return out
}

func base64DecodedLen(value string) int {
	n := base64.StdEncoding.DecodedLen(len(value))
	if strings.HasSuffix(value, "==") {
		n -= 2
	} else if strings.HasSuffix(value, "=") {
		n--
	}
	if n < 0 {
		return 0
	}
	return n
}
