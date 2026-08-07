package acpagent

import (
	"crypto/sha256"
	"encoding/base64"
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

	got, err := promptContentBlocks(content, acp.PromptCapabilities{
		Audio:           true,
		EmbeddedContext: true,
		Image:           true,
	})
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
	if got[4].Image == nil || got[4].Image.Uri == nil ||
		*got[4].Image.Uri != "file:///tmp/photo.jpg" || got[4].Image.MimeType != "image/jpeg" {
		t.Fatalf("image URI block = %#v", got[4])
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

	got, err := promptContentBlocks(content, acp.PromptCapabilities{EmbeddedContext: true})
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
			_, err := promptContentBlocks(test.content, acp.PromptCapabilities{
				Audio:           true,
				EmbeddedContext: true,
				Image:           true,
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("promptContentBlocks() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestPromptContentBlocksEnforcesAdvertisedCapabilities(t *testing.T) {
	tests := []struct {
		name         string
		part         *genai.Part
		capabilities acp.PromptCapabilities
		wantType     string
		wantErr      string
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
			wantErr: "does not support image",
		},
		{
			name:         "image advertised",
			part:         genai.NewPartFromBytes([]byte("image"), "image/png"),
			capabilities: acp.PromptCapabilities{Image: true},
			wantType:     "image",
		},
		{
			name: "image file requires capability",
			part: &genai.Part{FileData: &genai.FileData{
				FileURI:  "file:///tmp/photo.png",
				MIMEType: "image/png",
			}},
			wantErr: "does not support image",
		},
		{
			name:    "audio requires capability",
			part:    genai.NewPartFromBytes([]byte("audio"), "audio/mpeg"),
			wantErr: "does not support audio",
		},
		{
			name:         "audio advertised",
			part:         genai.NewPartFromBytes([]byte("audio"), "audio/mpeg"),
			capabilities: acp.PromptCapabilities{Audio: true},
			wantType:     "audio",
		},
		{
			name:    "embedded resource requires capability",
			part:    genai.NewPartFromBytes([]byte("document"), "application/pdf"),
			wantErr: "does not support embedded resource",
		},
		{
			name:         "embedded resource advertised",
			part:         genai.NewPartFromBytes([]byte("document"), "application/pdf"),
			capabilities: acp.PromptCapabilities{EmbeddedContext: true},
			wantType:     "resource",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			blocks, err := promptContentBlocks(&genai.Content{Parts: []*genai.Part{test.part}}, test.capabilities)
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
