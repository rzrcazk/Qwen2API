package upstream

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// NormalizeChatType maps API-facing aliases onto Qwen upstream chat types.
func NormalizeChatType(chatType string) string {
	if chatType == "image_gen" || chatType == "t2i" {
		return "t2i"
	}
	return chatType
}

// BuildChatPayload builds the Qwen /api/v2/chat/completions request body.
func BuildChatPayload(chatID, model, content string, hasCustomTools bool, files []map[string]any, chatType string, imageOptions map[string]any, thinkingEnabled *bool, enableSearch bool) map[string]any {
	if chatType == "" {
		chatType = "t2t"
	}
	ts := time.Now().Unix()
	isImage := chatType == "image_gen" || chatType == "t2i"
	isVideo := chatType == "t2v" || chatType == "i2v"
	featureConfig := map[string]any{}
	messageChatType := chatType
	subChatType := chatType
	messageMeta := map[string]any{"subChatType": chatType}
	var i2vInput map[string]any

	if isImage {
		ratio := imageRatio(imageOptions)
		featureConfig = map[string]any{
			"thinking_enabled": false, "output_schema": "phase", "auto_thinking": false,
			"thinking_mode": "off", "auto_search": false, "code_interpreter": false,
			"function_calling": false, "plugins_enabled": true, "image_generation": true,
			"default_aspect_ratio": ratio,
		}
		messageChatType = "t2t"
		subChatType = "t2i"
		messageMeta = map[string]any{"subChatType": "t2i", "mode": "image_generation", "aspectRatio": ratio, "size": ratio}
	} else if isVideo {
		ratio := imageRatio(imageOptions)
		featureConfig = map[string]any{
			"thinking_enabled": false, "output_schema": "phase", "auto_thinking": false,
			"thinking_mode": "off", "auto_search": false, "code_interpreter": false,
			"function_calling": false, "plugins_enabled": true, "video_generation": true,
			"default_aspect_ratio": ratio,
		}
		messageChatType = chatType
		subChatType = chatType
		messageMeta = map[string]any{"subChatType": chatType, "mode": "video_generation", "aspectRatio": ratio, "size": ratio}
		if chatType == "i2v" {
			if imageURL := firstFileURL(files); imageURL != "" {
				i2vInput = map[string]any{"img_url": imageURL}
				featureConfig["input"] = i2vInput
				messageMeta["input"] = i2vInput
			}
		}
	} else {
		thinking := true
		autoThinking := true
		thinkingMode := "Auto"
		if hasCustomTools {
			thinking = false
			autoThinking = false
			thinkingMode = "Disabled"
		}
		if thinkingEnabled != nil {
			thinking = *thinkingEnabled
			autoThinking = *thinkingEnabled
			if thinking {
				thinkingMode = "Auto"
			} else {
				thinkingMode = "Disabled"
			}
		}
		featureConfig = map[string]any{
			"thinking_enabled": thinking, "output_schema": "phase", "research_mode": "normal",
			"auto_thinking": autoThinking, "thinking_mode": thinkingMode, "thinking_format": "summary",
			"auto_search": enableSearch || chatType == "deep_research", "code_interpreter": false,
			"plugins_enabled": false, "function_calling": false, "enable_tools": false,
			"enable_function_call": false, "tool_choice": "none",
		}
	}

	if files == nil {
		files = []map[string]any{}
	}
	message := map[string]any{
		"fid": randomID(), "parentId": nil, "childrenIds": []string{randomID()},
		"role": "user", "content": content, "user_action": "chat", "files": files,
		"timestamp": ts, "models": []string{model}, "chat_type": messageChatType,
		"feature_config": featureConfig, "extra": map[string]any{"meta": messageMeta},
		"sub_chat_type": subChatType, "parent_id": nil,
	}
	if i2vInput != nil {
		message["input"] = i2vInput
	}
	payload := map[string]any{
		"stream": true, "version": "2.1", "incremental_output": true, "chat_id": chatID,
		"chat_mode": "normal", "model": model, "parent_id": nil,
		"messages":  []map[string]any{message},
		"timestamp": ts,
	}
	if i2vInput != nil {
		payload["input"] = i2vInput
	}
	if isImage || isVideo {
		payload["size"] = imageRatio(imageOptions)
	}
	return payload
}

func firstFileURL(files []map[string]any) string {
	for _, file := range files {
		if file == nil {
			continue
		}
		if url, _ := file["url"].(string); url != "" {
			return url
		}
		if imageURL, ok := file["image_url"].(map[string]any); ok {
			if url, _ := imageURL["url"].(string); url != "" {
				return url
			}
		}
	}
	return ""
}

func imageRatio(options map[string]any) string {
	for _, key := range []string{"ratio", "aspect_ratio", "aspectRatio"} {
		if v, ok := options[key].(string); ok && v != "" {
			return v
		}
	}
	return "1:1"
}

func randomID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "00000000000000000000000000000000"
	}
	return hex.EncodeToString(buf)
}
