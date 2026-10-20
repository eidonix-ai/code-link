// Package media rewrites a request's documents and images before
// translation (docs/DESIGN.md §4.4): documents become text through the
// DocumentReader, images for a model without vision become descriptions
// (or an "image omitted" note). Reads run concurrently, once per distinct
// document or image in the request.
//
// Port of the Swift MediaPreprocessor.swift and OpenAIMedia.swift.
package media

import (
	"context"

	"codelink/internal/hooks"
	"codelink/internal/upstream"
)

// AttachmentTextType marks a text block that came from media inside a
// tool_result: the Messages translator puts it in the user message after
// the tool messages.
const AttachmentTextType = "code_link_attachment_text"

// Context is what media processing needs for one request.
type Context struct {
	Reader hooks.DocumentReader // nil = documents become a note
	// Describer is set only when the vision role names a model.
	Describer      hooks.ImageDescriber
	ModelHasVision bool
	ModelID        string
	Fetcher        *upstream.Client
	Log            hooks.EventLog
	MaxFetchBytes  int64 // 0 = 32 MiB
}

// ProcessMessages rewrites an Anthropic Messages request's document and
// image blocks (top level and inside tool_result).
func ProcessMessages(ctx context.Context, req map[string]any, mc Context) map[string]any {
	return req // placeholder until the port lands
}

// ProcessChatMessages rewrites Chat Completions messages' image_url parts
// (for a model without vision) and file parts (PDFs).
func ProcessChatMessages(ctx context.Context, messages []any, mc Context) []any {
	return messages // placeholder
}

// ProcessResponsesInput rewrites Responses input items' input_image and
// input_file parts.
func ProcessResponsesInput(ctx context.Context, items []any, mc Context) []any {
	return items // placeholder
}
