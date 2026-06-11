package upstream

import "testing"

func TestBuildChatPayloadI2VIncludesImageInput(t *testing.T) {
	payload := BuildChatPayload(
		"chat-1",
		"qwen3.7-plus",
		"make a video",
		false,
		[]map[string]any{{"url": "https://cdn.example.com/ref.png"}},
		"i2v",
		map[string]any{"ratio": "9:16"},
		nil,
		false,
	)

	input, ok := payload["input"].(map[string]any)
	if !ok {
		t.Fatalf("payload input missing: %T", payload["input"])
	}
	if input["img_url"] != "https://cdn.example.com/ref.png" {
		t.Fatalf("input.img_url = %v", input["img_url"])
	}
	messages, _ := payload["messages"].([]map[string]any)
	if len(messages) != 1 {
		t.Fatalf("messages len = %d", len(messages))
	}
	if messages[0]["chat_type"] != "i2v" || messages[0]["sub_chat_type"] != "i2v" {
		t.Fatalf("message chat types = %v / %v", messages[0]["chat_type"], messages[0]["sub_chat_type"])
	}
	messageInput, _ := messages[0]["input"].(map[string]any)
	if messageInput["img_url"] != "https://cdn.example.com/ref.png" {
		t.Fatalf("message input.img_url = %v", messageInput["img_url"])
	}
	featureConfig, _ := messages[0]["feature_config"].(map[string]any)
	featureInput, _ := featureConfig["input"].(map[string]any)
	if featureInput["img_url"] != "https://cdn.example.com/ref.png" {
		t.Fatalf("feature_config input.img_url = %v", featureInput["img_url"])
	}
	extra, _ := messages[0]["extra"].(map[string]any)
	meta, _ := extra["meta"].(map[string]any)
	metaInput, _ := meta["input"].(map[string]any)
	if metaInput["img_url"] != "https://cdn.example.com/ref.png" {
		t.Fatalf("meta input.img_url = %v", metaInput["img_url"])
	}
}
