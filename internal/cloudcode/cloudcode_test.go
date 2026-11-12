package cloudcode

import (
	"encoding/json"
	"testing"
)

// catalog mirrors the shape of agy 1.5.3's fetchAvailableModels reply.
const catalog = `{
  "models": {
    "gemini-3.8-flash-high": {
      "maxTokens": 1048576,
      "maxOutputTokens": 65536,
      "modelExperiments": {"experiments": {
        "CASCADE_USE_EXPERIMENT_CHECKPOINTER": {"stringValue": "{\n \"strategy\": \"CHECKPOINT_STRATEGY_SAME_MODEL\",\n \"max_token_limit\": \"256000\",\n \"token_threshold\": \"50000\",\n \"enabled\": true\n}"},
        "retry_model_capacity_exhausted": {"boolValue": true}
      }}
    },
    "tab_flash_lite_preview": {"maxTokens": 16384, "maxOutputTokens": 4096},
    "gemini-4-hydrogen-disabled": {"displayName": "Gemini 4 Hydrogen", "maxTokens": 1048576, "disabled": true, "tagTitle": "Unavailable",
      "supportedMimeTypes": {"image/png": true}}
  },
  "defaultAgentModelId": "gemini-3.8-flash-high",
  "agentModelSorts": [{"displayName": "Recommended", "groups": [{"modelIds": ["gemini-3.8-flash-high", "gemini-4-hydrogen-disabled"]}]}]
}`

func TestAgentModelsKeepsTheRecordsAsSent(t *testing.T) {
	models, err := agentModels([]byte(catalog))
	if err != nil || len(models) != 2 {
		t.Fatalf("models %+v, err %v", models, err)
	}
	flash, hydrogen := models[0], models[1]
	// Before the rewrite: the upstream window, not disabledModelWindow.
	if flash.Slug != "gemini-3.8-flash-high" || flash.Disabled || flash.Fields["maxTokens"] != json.Number("1048576") {
		t.Fatalf("flash %+v", flash)
	}
	if _, ok := flash.Fields["modelExperiments"]; ok {
		t.Fatal("experiments must be left out")
	}
	if hydrogen.Slug != "gemini-4-hydrogen" || !hydrogen.Disabled || hydrogen.Fields["tagTitle"] != "Unavailable" || hydrogen.Fields["supportedMimeTypes"] != nil {
		t.Fatalf("hydrogen %+v", hydrogen)
	}
}

func TestRewriteCatalogDisablesCheckpointer(t *testing.T) {
	body, changed, err := rewriteCatalog([]byte(catalog))
	if err != nil || changed != 1 {
		t.Fatalf("changed %d, err %v", changed, err)
	}
	var got struct {
		Models map[string]struct {
			MaxTokens        int64 `json:"maxTokens"`
			MaxOutputTokens  int64 `json:"maxOutputTokens"`
			ModelExperiments struct {
				Experiments map[string]struct {
					StringValue string `json:"stringValue"`
					BoolValue   *bool  `json:"boolValue"`
				} `json:"experiments"`
			} `json:"modelExperiments"`
		} `json:"models"`
		Default string `json:"defaultAgentModelId"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	flash := got.Models["gemini-3.8-flash-high"]
	if flash.MaxTokens != disabledModelWindow || flash.MaxOutputTokens != 65536 {
		t.Fatalf("flash limits %d/%d", flash.MaxTokens, flash.MaxOutputTokens)
	}
	var config map[string]any
	if err := json.Unmarshal([]byte(flash.ModelExperiments.Experiments[checkpointerPrefix].StringValue), &config); err != nil {
		t.Fatal(err)
	}
	if config["enabled"] != false || config["max_token_limit"] != disabledTokenLimit || config["strategy"] != "CHECKPOINT_STRATEGY_SAME_MODEL" {
		t.Fatalf("checkpointer config %v", config)
	}
	if retry := flash.ModelExperiments.Experiments["retry_model_capacity_exhausted"].BoolValue; retry == nil || !*retry {
		t.Fatal("other experiments must stay")
	}
	if tab := got.Models["tab_flash_lite_preview"]; tab.MaxTokens != 16384 {
		t.Fatalf("a model without checkpointer keeps its window, got %d", tab.MaxTokens)
	}
	if got.Default != "gemini-3.8-flash-high" {
		t.Fatal("unrelated fields must stay")
	}
}

func TestRewriteCatalogWithoutCheckpointerKeepsBytes(t *testing.T) {
	raw := []byte(`{"models":{"m":{"maxTokens":5}}}`)
	body, changed, err := rewriteCatalog(raw)
	if err != nil || changed != 0 || string(body) != string(raw) {
		t.Fatalf("body %s, changed %d, err %v", body, changed, err)
	}
}

func TestRewriteCatalogRejectsInvalidJSON(t *testing.T) {
	if _, _, err := rewriteCatalog([]byte("{")); err == nil {
		t.Fatal("want an error")
	}
}
