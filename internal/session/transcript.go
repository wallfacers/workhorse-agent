package session

import (
	"encoding/json"

	"github.com/wallfacers/workhorse-agent/internal/provider"
)

// storedBlock is the on-disk JSON projection of a provider.ContentBlock. Field
// names are the stable persistence contract (add-project-sessions design D9);
// they are deliberately distinct from the history wire shape (D4), which the
// history endpoint maps onto separately. Signature is persisted so a hydrated
// session's thinking blocks survive an Anthropic API round-trip.
type storedBlock struct {
	Type         provider.BlockType `json:"type"`
	Text         string             `json:"text,omitempty"`
	ToolUseID    string             `json:"toolUseId,omitempty"`
	ToolName     string             `json:"toolName,omitempty"`
	Input        json.RawMessage    `json:"input,omitempty"`
	Output       string             `json:"output,omitempty"`
	IsError      bool               `json:"isError,omitempty"`
	Thinking     string             `json:"thinking,omitempty"`
	Signature    string             `json:"signature,omitempty"`
	RedactedData string             `json:"redactedData,omitempty"`
}

// marshalContent serialises a message's content blocks to the content_json
// column value. The output is always a JSON array ("[]" for an empty message).
func marshalContent(blocks []provider.ContentBlock) (string, error) {
	out := make([]storedBlock, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, storedBlock{
			Type:         b.Type,
			Text:         b.Text,
			ToolUseID:    b.ToolUseID,
			ToolName:     b.ToolName,
			Input:        b.Input,
			Output:       b.Output,
			IsError:      b.IsError,
			Thinking:     b.Thinking,
			Signature:    b.Signature,
			RedactedData: b.RedactedData,
		})
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// DecodeContent exposes the persisted content_json → []ContentBlock decoder to
// other packages (the history endpoint maps the blocks onto the wire parts[]
// shape). It is the read side of the same format Session persistence writes.
func DecodeContent(s string) ([]provider.ContentBlock, error) {
	return unmarshalContent(s)
}

// unmarshalContent is the inverse of marshalContent.
func unmarshalContent(s string) ([]provider.ContentBlock, error) {
	if s == "" {
		return nil, nil
	}
	var in []storedBlock
	if err := json.Unmarshal([]byte(s), &in); err != nil {
		return nil, err
	}
	out := make([]provider.ContentBlock, 0, len(in))
	for _, b := range in {
		out = append(out, provider.ContentBlock{
			Type:         b.Type,
			Text:         b.Text,
			ToolUseID:    b.ToolUseID,
			ToolName:     b.ToolName,
			Input:        b.Input,
			Output:       b.Output,
			IsError:      b.IsError,
			Thinking:     b.Thinking,
			Signature:    b.Signature,
			RedactedData: b.RedactedData,
		})
	}
	return out, nil
}
