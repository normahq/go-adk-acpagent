package acpagent

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"google.golang.org/genai"
)

func TestPromptContentBlocksPreservesOrderAndMedia(t *testing.T) {
	image := []byte("image")
	audio := []byte("audio")
	document := []byte("document")
	content := &genai.Content{
		Role: genai.RoleUser,
		Parts: []*genai.Part{
			genai.NewPartFromText(" hello "),
			genai.NewPartFromBytes(image, "image/png"),
			genai.NewPartFromBytes(audio, "audio/mpeg"),
			{InlineData: &genai.Blob{Data: document, MIMEType: "application/pdf"}},
			genai.NewPartFromURI("file:///tmp/photo.jpg", "image/jpeg"),
			{FileData: &genai.FileData{
				DisplayName: "report.pdf",
				FileURI:     "file:///tmp/report.pdf",
				MIMEType:    "application/pdf",
			}},
		},
	}

	got, err := promptContentBlocks(content, allPromptSupport())
	if err != nil {
		t.Fatalf("promptContentBlocks() error = %v", err)
	}
	if len(got) != 6 {
		t.Fatalf("promptContentBlocks() returned %d blocks, want 6", len(got))
	}
	if got[0].Text == nil || got[0].Text.Text != " hello " {
		t.Fatalf("text block = %#v", got[0])
	}
	if got[1].Image == nil || got[1].Image.Data != base64.StdEncoding.EncodeToString(image) ||
		got[1].Image.MimeType != "image/png" {
		t.Fatalf("image block = %#v", got[1])
	}
	if got[2].Audio == nil || got[2].Audio.Data != base64.StdEncoding.EncodeToString(audio) ||
		got[2].Audio.MimeType != "audio/mpeg" {
		t.Fatalf("audio block = %#v", got[2])
	}
	sum := sha256.Sum256(document)
	resource := got[3].Resource
	if resource == nil || resource.Resource.BlobResourceContents == nil {
		t.Fatalf("resource block = %#v", got[3])
	}
	if resource.Resource.BlobResourceContents.Blob != base64.StdEncoding.EncodeToString(document) ||
		resource.Resource.BlobResourceContents.Uri != fmt.Sprintf("urn:adk:inline:%x", sum) ||
		resource.Resource.BlobResourceContents.MimeType == nil ||
		*resource.Resource.BlobResourceContents.MimeType != "application/pdf" {
		t.Fatalf("resource contents = %#v", resource.Resource.BlobResourceContents)
	}
	if got[4].ResourceLink == nil || got[4].ResourceLink.Name != "photo.jpg" ||
		got[4].ResourceLink.Uri != "file:///tmp/photo.jpg" ||
		got[4].ResourceLink.MimeType == nil ||
		*got[4].ResourceLink.MimeType != "image/jpeg" {
		t.Fatalf("image resource link block = %#v", got[4])
	}
	if got[5].ResourceLink == nil || got[5].ResourceLink.Name != "report.pdf" ||
		got[5].ResourceLink.Uri != "file:///tmp/report.pdf" ||
		got[5].ResourceLink.MimeType == nil ||
		*got[5].ResourceLink.MimeType != "application/pdf" {
		t.Fatalf("resource link block = %#v", got[5])
	}
}

func TestPromptContentBlocksUsesResourceFallbacks(t *testing.T) {
	content := &genai.Content{Parts: []*genai.Part{
		{InlineData: &genai.Blob{Data: []byte("blob")}},
		{FileData: &genai.FileData{FileURI: "gs://bucket/path/readme.md"}},
		{FileData: &genai.FileData{FileURI: "opaque:"}},
	}}

	got, err := promptContentBlocks(content, allPromptSupport())
	if err != nil {
		t.Fatalf("promptContentBlocks() error = %v", err)
	}
	if got[0].Resource == nil || got[0].Resource.Resource.BlobResourceContents.MimeType != nil {
		t.Fatalf("untyped resource block = %#v", got[0])
	}
	if got[1].ResourceLink == nil || got[1].ResourceLink.Name != "readme.md" {
		t.Fatalf("derived resource link name = %#v", got[1])
	}
	if got[2].ResourceLink == nil || got[2].ResourceLink.Name != "resource" {
		t.Fatalf("fallback resource link name = %#v", got[2])
	}
}

func TestPromptContentBlocksCapabilityMatrix(t *testing.T) {
	const imageData = "image-bytes"
	const audioData = "audio-bytes"

	tests := []struct {
		name       string
		support    promptSupport
		part       *genai.Part
		want       string
		wantErr    bool
		wantMIME   string
		wantReason string
	}{
		{
			name:    "inline image native",
			support: promptSupport{image: true},
			part:    genai.NewPartFromBytes([]byte(imageData), "image/png"),
			want:    "image",
		},
		{
			name:       "inline image embedded fallback",
			support:    promptSupport{embeddedContext: true},
			part:       genai.NewPartFromBytes([]byte(imageData), "image/png"),
			want:       "resource",
			wantMIME:   "image/png",
			wantReason: "embedded",
		},
		{
			name:       "inline image unsupported",
			support:    promptSupport{},
			part:       genai.NewPartFromBytes([]byte(imageData), "image/png"),
			wantErr:    true,
			wantReason: "image",
		},
		{
			name:    "inline audio native",
			support: promptSupport{audio: true},
			part:    genai.NewPartFromBytes([]byte(audioData), "audio/mpeg"),
			want:    "audio",
		},
		{
			name:       "inline audio embedded fallback",
			support:    promptSupport{embeddedContext: true},
			part:       genai.NewPartFromBytes([]byte(audioData), "audio/mpeg"),
			want:       "resource",
			wantMIME:   "audio/mpeg",
			wantReason: "embedded",
		},
		{
			name:       "inline audio unsupported",
			support:    promptSupport{},
			part:       genai.NewPartFromBytes([]byte(audioData), "audio/mpeg"),
			wantErr:    true,
			wantReason: "audio",
		},
		{
			name:       "inline other embedded",
			support:    promptSupport{embeddedContext: true},
			part:       genai.NewPartFromBytes([]byte("document"), "application/pdf"),
			want:       "resource",
			wantMIME:   "application/pdf",
			wantReason: "embedded",
		},
		{
			name:       "inline other unsupported",
			support:    promptSupport{},
			part:       genai.NewPartFromBytes([]byte("document"), "application/pdf"),
			wantErr:    true,
			wantReason: "embeddedContext",
		},
		{
			name:       "file image resource link with image advertised",
			support:    promptSupport{image: true},
			part:       genai.NewPartFromURI("file:///tmp/photo.jpg", "image/jpeg"),
			want:       "resource_link",
			wantReason: "baseline",
		},
		{
			name:       "file audio resource link baseline",
			support:    promptSupport{},
			part:       genai.NewPartFromURI("file:///tmp/voice.ogg", "audio/ogg"),
			want:       "resource_link",
			wantReason: "baseline",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := promptContentBlocks(&genai.Content{Parts: []*genai.Part{test.part}}, test.support)
			if test.wantErr {
				if err == nil {
					t.Fatal("promptContentBlocks() error = nil, want capability error")
				}
				if !errors.Is(err, ErrPromptCapabilityUnsupported) {
					t.Fatalf("promptContentBlocks() error = %v, want ErrPromptCapabilityUnsupported", err)
				}
				var capabilityErr *PromptCapabilityError
				if !errors.As(err, &capabilityErr) {
					t.Fatalf("promptContentBlocks() error = %v, want PromptCapabilityError", err)
				}
				if !strings.Contains(err.Error(), test.wantReason) {
					t.Fatalf("capability error = %q, want %q", err, test.wantReason)
				}
				for _, secret := range []string{"image-bytes", "audio-bytes", "file:///tmp"} {
					if strings.Contains(err.Error(), secret) {
						t.Fatalf("capability error leaked %q: %q", secret, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("promptContentBlocks() error = %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("promptContentBlocks() returned %d blocks, want 1", len(got))
			}
			if gotKind := testContentBlockKind(got[0]); gotKind != test.want {
				t.Fatalf("content block kind = %q, want %q", gotKind, test.want)
			}
			if test.wantMIME != "" {
				if got[0].Resource == nil || got[0].Resource.Resource.BlobResourceContents == nil ||
					got[0].Resource.Resource.BlobResourceContents.MimeType == nil ||
					*got[0].Resource.Resource.BlobResourceContents.MimeType != test.wantMIME {
					t.Fatalf("embedded resource MIME = %#v, want %q", got[0].Resource, test.wantMIME)
				}
			}
		})
	}
}

func TestValidatePromptBlocksUsesBaselineAndOptionalCapabilities(t *testing.T) {
	baseline := []acp.ContentBlock{
		acp.TextBlock("text"),
		acp.ResourceLinkBlock("document", "file:///tmp/document.pdf"),
	}
	if err := validatePromptBlocks(baseline, promptSupport{}); err != nil {
		t.Fatalf("validatePromptBlocks(baseline) error = %v", err)
	}

	for _, test := range []struct {
		name    string
		block   acp.ContentBlock
		support promptSupport
		want    string
	}{
		{name: "image", block: acp.ImageBlock("aW1hZ2U=", "image/png"), support: promptSupport{}, want: "image"},
		{name: "audio", block: acp.AudioBlock("YXVkaW8=", "audio/mpeg"), support: promptSupport{}, want: "audio"},
		{name: "resource", block: acp.ResourceBlock(acp.EmbeddedResourceResource{TextResourceContents: &acp.TextResourceContents{Text: "context", Uri: "urn:test"}}), support: promptSupport{}, want: "embeddedContext"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validatePromptBlocks([]acp.ContentBlock{test.block}, test.support)
			if err == nil || !errors.Is(err, ErrPromptCapabilityUnsupported) {
				t.Fatalf("validatePromptBlocks() error = %v, want capability error", err)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validatePromptBlocks() error = %q, want %q", err, test.want)
			}
		})
	}
}

func TestValidatePromptBlocksAcceptsOptionalCapabilities(t *testing.T) {
	blocks := []acp.ContentBlock{
		acp.ImageBlock("aW1hZ2U=", "image/png"),
		acp.AudioBlock("YXVkaW8=", "audio/mpeg"),
		acp.ResourceBlock(acp.EmbeddedResourceResource{TextResourceContents: &acp.TextResourceContents{Text: "context", Uri: "urn:test"}}),
	}
	if err := validatePromptBlocks(blocks, allPromptSupport()); err != nil {
		t.Fatalf("validatePromptBlocks(all capabilities) error = %v", err)
	}
}

func TestPromptCapabilityErrorAndMIMEClassificationAreRedacted(t *testing.T) {
	var nilError *PromptCapabilityError
	if got := nilError.Error(); got != ErrPromptCapabilityUnsupported.Error() {
		t.Fatalf("nil PromptCapabilityError.Error() = %q, want %q", got, ErrPromptCapabilityUnsupported)
	}

	for _, test := range []struct {
		mime string
		want string
	}{
		{mime: "", want: "untyped"},
		{mime: "image/png", want: "image"},
		{mime: "audio/ogg", want: "audio"},
		{mime: "text/plain", want: "text"},
		{mime: "application/pdf", want: "application"},
		{mime: "private/secret", want: "other"},
	} {
		t.Run(test.want, func(t *testing.T) {
			if got := mimeClass(test.mime); got != test.want {
				t.Fatalf("mimeClass(%q) = %q, want %q", test.mime, got, test.want)
			}
		})
	}

	err := (&PromptCapabilityError{
		PartKind:           "inline_data",
		MIMEClass:          "other",
		RequiredCapability: "embeddedContext",
		Fallback:           "provide FileData",
	}).Error()
	if !strings.Contains(err, "inline_data") || !strings.Contains(err, "embeddedContext") || !strings.Contains(err, "provide FileData") {
		t.Fatalf("capability error = %q, want structural details", err)
	}
	for _, secret := range []string{"private/secret", "file:///tmp", "secret-bytes"} {
		if strings.Contains(err, secret) {
			t.Fatalf("capability error leaked %q: %q", secret, err)
		}
	}
}

func testContentBlockKind(block acp.ContentBlock) string {
	switch {
	case block.Text != nil:
		return "text"
	case block.Image != nil:
		return "image"
	case block.Audio != nil:
		return "audio"
	case block.ResourceLink != nil:
		return "resource_link"
	case block.Resource != nil:
		return "resource"
	default:
		return "unknown"
	}
}

func TestPromptContentBlocksRejectsInvalidParts(t *testing.T) {
	tests := []struct {
		name    string
		content *genai.Content
		want    string
	}{
		{name: "nil content", want: "prompt content is empty"},
		{name: "no parts", content: &genai.Content{}, want: "prompt content is empty"},
		{name: "nil part", content: &genai.Content{Parts: []*genai.Part{nil}}, want: "part is nil"},
		{name: "whitespace text", content: &genai.Content{Parts: []*genai.Part{{Text: " "}}}, want: "prompt content is empty"},
		{name: "empty part", content: &genai.Content{Parts: []*genai.Part{{}}}, want: "no supported content field"},
		{name: "empty inline data", content: &genai.Content{Parts: []*genai.Part{{InlineData: &genai.Blob{}}}}, want: "inline data is empty"},
		{name: "empty file URI", content: &genai.Content{Parts: []*genai.Part{{FileData: &genai.FileData{}}}}, want: "file URI is empty"},
		{
			name: "multiple fields",
			content: &genai.Content{Parts: []*genai.Part{{
				Text:       "text",
				InlineData: &genai.Blob{Data: []byte("image"), MIMEType: "image/png"},
			}}},
			want: "multiple content fields",
		},
		{name: "function call", content: &genai.Content{Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{}}}}, want: "function_call"},
		{name: "function response", content: &genai.Content{Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{}}}}, want: "function_response"},
		{name: "executable code", content: &genai.Content{Parts: []*genai.Part{{ExecutableCode: &genai.ExecutableCode{}}}}, want: "executable_code"},
		{name: "code result", content: &genai.Content{Parts: []*genai.Part{{CodeExecutionResult: &genai.CodeExecutionResult{}}}}, want: "code_execution_result"},
		{name: "tool call", content: &genai.Content{Parts: []*genai.Part{{ToolCall: &genai.ToolCall{}}}}, want: "tool_call"},
		{name: "tool response", content: &genai.Content{Parts: []*genai.Part{{ToolResponse: &genai.ToolResponse{}}}}, want: "tool_response"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := promptContentBlocks(test.content, allPromptSupport())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("promptContentBlocks() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestPromptContentBlocksEnforcesAdvertisedCapabilities(t *testing.T) {
	tests := []struct {
		name     string
		part     *genai.Part
		support  promptSupport
		wantType string
		wantErr  string
	}{
		{
			name:     "text is baseline",
			part:     genai.NewPartFromText("hello"),
			wantType: "text",
		},
		{
			name: "resource link is baseline",
			part: &genai.Part{FileData: &genai.FileData{
				DisplayName: "report.pdf",
				FileURI:     "file:///tmp/report.pdf",
				MIMEType:    "application/pdf",
			}},
			wantType: "resource_link",
		},
		{
			name:    "image requires capability",
			part:    genai.NewPartFromBytes([]byte("image"), "image/png"),
			wantErr: "promptCapabilities.image",
		},
		{
			name:     "image advertised",
			part:     genai.NewPartFromBytes([]byte("image"), "image/png"),
			support:  promptSupport{image: true},
			wantType: "image",
		},
		{
			name: "image file requires capability",
			part: &genai.Part{FileData: &genai.FileData{
				FileURI:  "file:///tmp/photo.png",
				MIMEType: "image/png",
			}},
			wantType: "resource_link",
		},
		{
			name:    "audio requires capability",
			part:    genai.NewPartFromBytes([]byte("audio"), "audio/mpeg"),
			wantErr: "promptCapabilities.audio",
		},
		{
			name:     "audio advertised",
			part:     genai.NewPartFromBytes([]byte("audio"), "audio/mpeg"),
			support:  promptSupport{audio: true},
			wantType: "audio",
		},
		{
			name:    "embedded resource requires capability",
			part:    genai.NewPartFromBytes([]byte("document"), "application/pdf"),
			wantErr: "promptCapabilities.embeddedContext",
		},
		{
			name:     "embedded resource advertised",
			part:     genai.NewPartFromBytes([]byte("document"), "application/pdf"),
			support:  promptSupport{embeddedContext: true},
			wantType: "resource",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			blocks, err := promptContentBlocks(&genai.Content{Parts: []*genai.Part{test.part}}, test.support)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("promptContentBlocks() error = %v, want containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("promptContentBlocks() error = %v", err)
			}
			if len(blocks) != 1 {
				t.Fatalf("promptContentBlocks() returned %d blocks, want 1", len(blocks))
			}
			if got := promptBlockLogs(blocks)[0].Type; got != test.wantType {
				t.Fatalf("prompt block type = %q, want %q", got, test.wantType)
			}
		})
	}
}

func TestPrependInstructionsToContentUsesSeparateBlock(t *testing.T) {
	prompt := []acp.ContentBlock{
		acp.TextBlock("hello"),
		acp.ImageBlock(base64.StdEncoding.EncodeToString([]byte("image")), "image/png"),
	}
	got := prependInstructionsToContent(" guide ", prompt)
	if len(got) != 3 || got[0].Text == nil ||
		got[0].Text.Text != "guide\n\nUser message:\n" ||
		got[1].Text == nil || got[1].Text.Text != "hello" ||
		got[2].Image == nil {
		t.Fatalf("prependInstructionsToContent() = %#v", got)
	}

	withoutInstructions := prependInstructionsToContent("", prompt)
	if len(withoutInstructions) != len(prompt) || &withoutInstructions[0] == &prompt[0] {
		t.Fatalf("prependInstructionsToContent(empty) did not clone prompt")
	}
}

func TestPromptBlockLogsContainsOnlyTypesAndLengths(t *testing.T) {
	mimeType := "text/plain"
	blocks := []acp.ContentBlock{
		acp.TextBlock("secret"),
		acp.ImageBlock(base64.StdEncoding.EncodeToString([]byte("image")), "image/png"),
		acp.AudioBlock(base64.StdEncoding.EncodeToString([]byte("audio")), "audio/mpeg"),
		acp.ResourceLinkBlock("name", "file:///secret"),
		acp.ResourceBlock(acp.EmbeddedResourceResource{
			BlobResourceContents: &acp.BlobResourceContents{
				Blob:     base64.StdEncoding.EncodeToString([]byte("blob")),
				MimeType: &mimeType,
				Uri:      "urn:blob",
			},
		}),
		acp.ResourceBlock(acp.EmbeddedResourceResource{
			TextResourceContents: &acp.TextResourceContents{
				Text: "resource text",
				Uri:  "urn:text",
			},
		}),
		{},
	}
	got := promptBlockLogs(blocks)
	want := []promptBlockLog{
		{Type: "text", Bytes: 6},
		{Type: "image", Bytes: 5},
		{Type: "audio", Bytes: 5},
		{Type: "resource_link"},
		{Type: "resource", Bytes: 4},
		{Type: "resource", Bytes: 13},
		{Type: "unknown"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("promptBlockLogs() = %+v, want %+v", got, want)
	}
}
