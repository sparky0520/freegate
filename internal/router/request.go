package router

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

// ParsedRequest holds the analyzed fields of an incoming chat completion request.
type ParsedRequest struct {
	Model           string
	Stream          bool
	HasTools        bool
	HasImages       bool
	EstimatedTokens int
	MaxTokens       int
	IsComplex       bool
	SessionID       string
	RawBody         []byte
	ReqMap          map[string]json.RawMessage
}

// ParseRequest inspects the request body and headers.
func ParseRequest(r *http.Request, bodyBytes []byte) (*ParsedRequest, error) {
	var reqMap map[string]json.RawMessage
	if err := json.Unmarshal(bodyBytes, &reqMap); err != nil {
		return nil, err
	}

	p := &ParsedRequest{
		RawBody: bodyBytes,
		ReqMap:  reqMap,
	}

	// 1. Model
	if mRaw, ok := reqMap["model"]; ok {
		_ = json.Unmarshal(mRaw, &p.Model)
	}

	// 2. Stream
	if sRaw, ok := reqMap["stream"]; ok {
		_ = json.Unmarshal(sRaw, &p.Stream)
	}

	// 3. Tools / Functions
	if tRaw, ok := reqMap["tools"]; ok && len(tRaw) > 0 {
		var tools []any
		if err := json.Unmarshal(tRaw, &tools); err == nil && len(tools) > 0 {
			p.HasTools = true
		}
	}
	if !p.HasTools {
		if fRaw, ok := reqMap["functions"]; ok && len(fRaw) > 0 {
			var functions []any
			if err := json.Unmarshal(fRaw, &functions); err == nil && len(functions) > 0 {
				p.HasTools = true
			}
		}
	}

	// 4. Max Tokens
	if mtRaw, ok := reqMap["max_tokens"]; ok {
		_ = json.Unmarshal(mtRaw, &p.MaxTokens)
	} else if mctRaw, ok := reqMap["max_completion_tokens"]; ok {
		_ = json.Unmarshal(mctRaw, &p.MaxTokens)
	}
	if p.MaxTokens <= 0 {
		p.MaxTokens = 1024
	}

	// 5. Estimated Tokens: len(bodyBytes)/4 is standard for v1
	p.EstimatedTokens = len(bodyBytes) / 4

	// 6. Inspect messages for images, code fences, and session hash
	var messages []map[string]any
	var systemPrompt, firstUserMessage string

	if msgRaw, ok := reqMap["messages"]; ok {
		if err := json.Unmarshal(msgRaw, &messages); err == nil {
			for _, msg := range messages {
				role, _ := msg["role"].(string)
				content := msg["content"]

				// String content
				if strContent, ok := content.(string); ok {
					if strings.Contains(strContent, "```") {
						p.IsComplex = true
					}
					if role == "system" && systemPrompt == "" {
						systemPrompt = strContent
					} else if role == "user" && firstUserMessage == "" {
						firstUserMessage = strContent
					}
				}

				// Multi-part content
				if contentSlice, ok := content.([]any); ok {
					for _, part := range contentSlice {
						if partMap, ok := part.(map[string]any); ok {
							partType, _ := partMap["type"].(string)
							if partType == "image_url" || partType == "image" || partType == "input_image" {
								p.HasImages = true
							}
							if _, ok := partMap["image_url"]; ok {
								p.HasImages = true
							}
							if partType == "text" {
								if textStr, ok := partMap["text"].(string); ok {
									if strings.Contains(textStr, "```") {
										p.IsComplex = true
									}
									if role == "system" && systemPrompt == "" {
										systemPrompt = textStr
									} else if role == "user" && firstUserMessage == "" {
										firstUserMessage = textStr
									}
								}
							}
						}
					}
				}
			}
		}
	}

	// Complexity calculation:
	// "short and simple (below ~2k estimated tokens, no tools, no code fences) prefers tier 1 then up;
	// long, code-heavy, or tool-using prefers tier 3 then down."
	if p.EstimatedTokens >= 2000 || p.HasTools || p.HasImages {
		p.IsComplex = true
	}

	// 7. Session ID: X-Session-Id header if present, otherwise hash of system + first user message
	if sessHeader := r.Header.Get("X-Session-Id"); sessHeader != "" {
		p.SessionID = sessHeader
	} else if systemPrompt != "" || firstUserMessage != "" {
		h := sha256.New()
		h.Write([]byte(systemPrompt))
		h.Write([]byte(firstUserMessage))
		p.SessionID = hex.EncodeToString(h.Sum(nil))
	}

	return p, nil
}
