package llmtraining

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
)

// Adapters must match the architecture (module layout) of the primary model,
// so candidates train only its Qwen3.5 image-text architecture.
const (
	modelFamily       = "qwen3_5"
	modelTextFamily   = "qwen3_5_text"
	modelArchitecture = "Qwen3_5ForConditionalGeneration"

	processorClass      = "Qwen3VLProcessor"
	videoProcessorClass = "Qwen3VLVideoProcessor"

	imageProcessorName  = "preprocessor_config.json"
	videoProcessorName  = "video_preprocessor_config.json"
	processorConfigName = "processor_config.json"
)

// imageProcessorClasses are the classes that the pinned snapshots and the
// locked transformers name for the image processor.
var imageProcessorClasses = []string{"Qwen2VLImageProcessorFast", "Qwen2VLImageProcessor"}

// tokenizerRootRejected names processor-related files that the processor
// loader would read from the tokenizer snapshot but that the candidate does
// not inspect.
var tokenizerRootRejected = []string{"chat_template.json", "config.json"}

func inspectArchitecture(config map[string]json.RawMessage) error {
	var family string
	var architectures []string
	var text struct {
		ModelType string `json:"model_type"`
	}
	if json.Unmarshal(config["model_type"], &family) != nil || family != modelFamily ||
		json.Unmarshal(config["architectures"], &architectures) != nil || len(architectures) != 1 || architectures[0] != modelArchitecture ||
		json.Unmarshal(config["text_config"], &text) != nil || text.ModelType != modelTextFamily {
		return errors.New("candidate supports only the Qwen3.5 image-text architecture (" + modelArchitecture + ")")
	}
	return nil
}

// inspectProcessors checks the processor configuration in the tokenizer
// snapshot, which training uses as the processor root. The image and video
// processor files are required; a combined processor_config.json is checked
// when present.
func inspectProcessors(inventory Inventory) error {
	for _, name := range tokenizerRootRejected {
		if inventory.Files[name] != "" {
			return errors.New("candidate tokenizer snapshot must not contain " + name)
		}
	}
	for _, name := range []string{imageProcessorName, videoProcessorName} {
		if inventory.Files[name] == "" {
			return errors.New("candidate tokenizer snapshot requires " + name)
		}
	}
	for _, name := range []string{imageProcessorName, videoProcessorName, processorConfigName} {
		if inventory.Files[name] == "" {
			continue
		}
		if err := checkProcessorFile(filepath.Join(inventory.Root, name), name, "candidate"); err != nil {
			return err
		}
	}
	return nil
}

// checkExportProcessors requires the processor that training saved with the
// adapter, because training always loads one. It accepts a combined
// processor_config.json, or both split files, and checks every file present.
func checkExportProcessors(directory string) error {
	present := map[string]bool{}
	for _, name := range []string{imageProcessorName, videoProcessorName, processorConfigName} {
		path := filepath.Join(directory, name)
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		}
		present[name] = true
		if err := checkProcessorFile(path, name, "training export"); err != nil {
			return err
		}
	}
	if present[imageProcessorName] != present[videoProcessorName] || (!present[processorConfigName] && !present[imageProcessorName]) {
		return errors.New("training export requires a complete processor: " + processorConfigName + ", or both " + imageProcessorName + " and " + videoProcessorName)
	}
	return nil
}

// checkProcessorFile requires the Qwen3VLProcessor classes and no auto_map, so
// the file selects only these processor classes of the locked transformers
// and loads no remote code. The error names the field that failed.
func checkProcessorFile(path, name, owner string) error {
	config, err := readJSONObject(path, owner+" processor configuration")
	if err != nil {
		return err
	}
	if field := processorMismatch(config, name); field != "" {
		return errors.New(owner + " processor configuration " + name + ": unsupported " + field + "; the processor must be Qwen3VLProcessor without auto_map")
	}
	return nil
}

// processorMismatch returns the first field that does not hold the expected
// value, or "" when the file is accepted.
func processorMismatch(config map[string]json.RawMessage, name string) string {
	if _, exists := config["auto_map"]; exists {
		return "auto_map"
	}
	if !stringIs(config["processor_class"], processorClass) {
		return "processor_class"
	}
	switch name {
	case imageProcessorName:
		return imageMismatch(config, "")
	case videoProcessorName:
		return videoMismatch(config, "")
	}
	var image, video map[string]json.RawMessage
	if json.Unmarshal(config["image_processor"], &image) != nil || image == nil {
		return "image_processor"
	}
	if json.Unmarshal(config["video_processor"], &video) != nil || video == nil {
		return "video_processor"
	}
	if field := imageMismatch(image, "image_processor."); field != "" {
		return field
	}
	return videoMismatch(video, "video_processor.")
}

func imageMismatch(config map[string]json.RawMessage, prefix string) string {
	if _, exists := config["auto_map"]; exists {
		return prefix + "auto_map"
	}
	var class string
	if json.Unmarshal(config["image_processor_type"], &class) != nil || !slices.Contains(imageProcessorClasses, class) {
		return prefix + "image_processor_type"
	}
	return ""
}

func videoMismatch(config map[string]json.RawMessage, prefix string) string {
	if _, exists := config["auto_map"]; exists {
		return prefix + "auto_map"
	}
	if !stringIs(config["video_processor_type"], videoProcessorClass) {
		return prefix + "video_processor_type"
	}
	return ""
}

func stringIs(raw json.RawMessage, want string) bool {
	var value string
	return json.Unmarshal(raw, &value) == nil && value == want
}
