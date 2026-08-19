package official

import (
	"encoding/json"
	"testing"
)

func TestToolChoiceRequiresCall(t *testing.T) {
	forcedFunction := &ToolChoice{Type: "function", Function: &ToolChoiceFunction{Name: "bash"}}
	tests := []struct {
		name   string
		choice *ToolChoice
		want   bool
	}{
		{name: "nil", choice: nil, want: false},
		{name: "auto", choice: &ToolChoice{Type: "auto"}, want: false},
		{name: "none", choice: &ToolChoice{Type: "none"}, want: false},
		{name: "required", choice: &ToolChoice{Type: "required"}, want: true},
		{name: "any alias", choice: &ToolChoice{Type: "any"}, want: true},
		{name: "forced function", choice: forcedFunction, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.choice.RequiresCall(); got != tt.want {
				t.Fatalf("RequiresCall() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestToolChoiceNoneIsForcedNoneWithoutRequiringCall(t *testing.T) {
	choice := &ToolChoice{Type: "none"}
	if !choice.IsForcedNone() {
		t.Fatal("tool_choice=none must be recognized as forced none")
	}
	if choice.RequiresCall() {
		t.Fatal("tool_choice=none must never require a tool call")
	}
}

func TestMessageContentFilesPreservesOpenCodeFileDataURL(t *testing.T) {
	const dataURL = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
	payload := `{
		"role":"user",
		"content":[
			{"type":"text","text":"inspect this image"},
			{"type":"file","mime":"image/png","filename":"pixel.png","url":"` + dataURL + `"}
		]
	}`

	var message APIMessage
	if err := json.Unmarshal([]byte(payload), &message); err != nil {
		t.Fatalf("unmarshal OpenCode message: %v", err)
	}
	if got := message.Text(); got != "inspect this image" {
		t.Fatalf("Text() = %q, want inspect this image", got)
	}

	files := message.Files()
	if len(files) != 1 {
		t.Fatalf("Files() count = %d, want 1; parsed content: %#v", len(files), message.Content)
	}
	file := files[0]
	if file.Source != dataURL {
		t.Fatalf("Source = %q, want original data URL", file.Source)
	}
	if got := firstNonEmpty(file.Filename, file.FileName, file.Name); got != "pixel.png" {
		t.Fatalf("filename = %q, want pixel.png", got)
	}
	if got := firstNonEmpty(file.MIMEType, file.MimeType); got != "image/png" {
		t.Fatalf("mime type = %q, want image/png", got)
	}
}

func TestMessageContentFilesPreservesOpenCodeImageURLWireFormat(t *testing.T) {
	const dataURL = "data:image/png;base64,iVBORw0KGgo="
	payload := `{
		"role":"user",
		"content":[
			{"type":"text","text":"inspect this image"},
			{"type":"image_url","image_url":{"url":"` + dataURL + `"}}
		]
	}`

	var message APIMessage
	if err := json.Unmarshal([]byte(payload), &message); err != nil {
		t.Fatalf("unmarshal OpenCode wire message: %v", err)
	}
	files := message.Files()
	if len(files) != 1 {
		t.Fatalf("Files() count = %d, want 1", len(files))
	}
	if files[0].Source != dataURL || files[0].Filename != "image.png" || files[0].MimeType != "image/png" {
		t.Fatalf("unexpected image_url attachment: %#v", files[0])
	}
}

func TestGuessImageFilenameUsesDataURLMIME(t *testing.T) {
	tests := map[string]string{
		"data:image/png;base64,AA==":  "image.png",
		"data:image/jpeg;base64,AA==": "image.jpg",
		"data:image/webp;base64,AA==": "image.webp",
		"data:image/gif;base64,AA==":  "image.gif",
	}
	for dataURL, want := range tests {
		if got := guessImageFilename(dataURL); got != want {
			t.Errorf("guessImageFilename(%q) = %q, want %q", dataURL, got, want)
		}
	}
}
