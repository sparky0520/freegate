package router

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
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
	MaxTokensKey    string // field that carried max_tokens ("" if the client set none; we never inject one)
	IsComplex       bool
	SessionID       string
	RawBody         []byte
	ReqMap          map[string]json.RawMessage
}

var dynamicTimeRe = regexp.MustCompile(`(?i)(?:current(?:\s+date)?(?:\s+time)?|timestamp|time is|step\s+\d+|today is|\b202\d-[01]\d-[0-3]\d\b)`)

// extractStableSystemPrompt strips volatile lines (timestamps, step counters) so agents
// with dynamic system prompts maintain session stickiness across turns.
func extractStableSystemPrompt(s string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	var stable []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if dynamicTimeRe.MatchString(trimmed) {
			continue
		}
		stable = append(stable, line)
	}
	if len(stable) == 0 {
		return s
	}
	res := strings.Join(stable, "\n")
	if len(res) > 512 {
		return res[:512]
	}
	return res
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
		p.MaxTokensKey = "max_tokens"
	} else if mctRaw, ok := reqMap["max_completion_tokens"]; ok {
		_ = json.Unmarshal(mctRaw, &p.MaxTokens)
		p.MaxTokensKey = "max_completion_tokens"
	}
	if p.MaxTokens <= 0 {
		p.MaxTokensKey = "" // null/zero/invalid: treat as not set by the client
	}
	if p.MaxTokens <= 0 {
		p.MaxTokens = 1024
	}

	// 5. Inspect messages for images, code fences, and session hash
	var messages []map[string]any
	var systemPrompt, firstUserMessage string
	totalContentChars := 0
	hasCodeFences := false

	if msgRaw, ok := reqMap["messages"]; ok {
		if err := json.Unmarshal(msgRaw, &messages); err == nil {
			for _, msg := range messages {
				role, _ := msg["role"].(string)
				content := msg["content"]

				// String content
				if strContent, ok := content.(string); ok {
					totalContentChars += len(strContent)
					if strings.Contains(strContent, "```") {
						p.IsComplex = true
						hasCodeFences = true
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
									totalContentChars += len(textStr)
									if strings.Contains(textStr, "```") {
										p.IsComplex = true
										hasCodeFences = true
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

	// Base token estimation:
	// Code tokenizes denser (~3 chars/token), general text/JSON overhead ~4 chars/token.
	if totalContentChars > 0 && hasCodeFences {
		// Code-heavy requests tokenize around 3 chars per token
		p.EstimatedTokens = totalContentChars / 3
		if p.EstimatedTokens < len(bodyBytes)/4 {
			p.EstimatedTokens = len(bodyBytes) / 4
		}
	} else {
		p.EstimatedTokens = len(bodyBytes) / 4
	}
	if p.EstimatedTokens <= 0 {
		p.EstimatedTokens = 1
	}

	// Complexity calculation:
	// "short and simple (below ~2k estimated tokens, no tools, no code fences) prefers tier 1 then up;
	// long, code-heavy, or tool-using prefers tier 3 then down."
	if p.EstimatedTokens >= 2000 || p.HasTools || p.HasImages {
		p.IsComplex = true
	}

	// 7. Session ID:
	// Priority 1: X-Session-Id header if present
	if sessHeader := r.Header.Get("X-Session-Id"); sessHeader != "" {
		p.SessionID = sessHeader
	} else if uRaw, ok := reqMap["user"]; ok && len(uRaw) > 0 {
		var uStr string
		if err := json.Unmarshal(uRaw, &uStr); err == nil && uStr != "" {
			p.SessionID = uStr
		}
	} else if sRaw, ok := reqMap["session_id"]; ok && len(sRaw) > 0 {
		var sStr string
		if err := json.Unmarshal(sRaw, &sStr); err == nil && sStr != "" {
			p.SessionID = sStr
		}
	}

	// Fallback: hash stable parts of system prompt and first user message
	if p.SessionID == "" && (systemPrompt != "" || firstUserMessage != "") {
		stableSystem := extractStableSystemPrompt(systemPrompt)
		h := sha256.New()
		h.Write([]byte(stableSystem))
		h.Write([]byte(firstUserMessage))
		p.SessionID = hex.EncodeToString(h.Sum(nil))
	}

	return p, nil
}
