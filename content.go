package acpagent

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/url"
	"path"
	"strings"

	acp "github.com/coder/acp-go-sdk"
	"google.golang.org/genai"
)

func promptContentBlocks(content *genai.Content) ([]acp.ContentBlock, error) {
	if content == nil {
		return nil, fmt.Errorf("prompt content is empty")
	}

	blocks := make([]acp.ContentBlock, 0, len(content.Parts))
	for i, part := range content.Parts {
		block, include, err := promptContentBlock(part)
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

func promptContentBlock(part *genai.Part) (acp.ContentBlock, bool, error) {
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
		return inlineDataBlock(part.InlineData)
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

func inlineDataBlock(blob *genai.Blob) (acp.ContentBlock, bool, error) {
	if blob == nil || len(blob.Data) == 0 {
		return acp.ContentBlock{}, false, fmt.Errorf("inline data is empty")
	}

	mimeType := strings.ToLower(strings.TrimSpace(blob.MIMEType))
	data := base64.StdEncoding.EncodeToString(blob.Data)
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		return acp.ImageBlock(data, mimeType), true, nil
	case strings.HasPrefix(mimeType, "audio/"):
		return acp.AudioBlock(data, mimeType), true, nil
	default:
		sum := sha256.Sum256(blob.Data)
		contents := &acp.BlobResourceContents{
			Blob: data,
			Uri:  fmt.Sprintf("urn:adk:inline:%x", sum),
		}
		if mimeType != "" {
			contents.MimeType = &mimeType
		}
		return acp.ResourceBlock(acp.EmbeddedResourceResource{
			BlobResourceContents: contents,
		}), true, nil
	}
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
	if strings.HasPrefix(mimeType, "image/") {
		return acp.ContentBlock{
			Image: &acp.ContentBlockImage{
				Type:     "image",
				MimeType: mimeType,
				Uri:      &uri,
			},
		}, true, nil
	}

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
