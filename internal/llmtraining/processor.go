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

// checkExportProcessors checks the processor files that training saved with
// the adapter.
func checkExportProcessors(directory string) error {
	for _, name := range []string{imageProcessorName, videoProcessorName, processorConfigName} {
		path := filepath.Join(directory, name)
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := checkProcessorFile(path, name, "training export"); err != nil {
			return err
		}
	}
	return nil
}

// checkProcessorFile requires the Qwen3VLProcessor classes and no auto_map, so
// the file selects only these processor classes of the locked transformers
// and loads no remote code.
func checkProcessorFile(path, name, owner string) error {
	config, err := readJSONObject(path, owner+" processor configuration")
	if err != nil {
		return err
	}
	invalid := errors.New(owner + " processor configuration " + name + " must name the Qwen3VLProcessor classes without auto_map")
	if !processorClasses(config, name) {
		return invalid
	}
	return nil
}

func processorClasses(config map[string]json.RawMessage, name string) bool {
	if _, exists := config["auto_map"]; exists || !stringIs(config["processor_class"], processorClass) {
		return false
	}
	switch name {
	case imageProcessorName:
		return imageClass(config)
	case videoProcessorName:
		return videoClass(config)
	}
	var image, video map[string]json.RawMessage
	if json.Unmarshal(config["image_processor"], &image) != nil || json.Unmarshal(config["video_processor"], &video) != nil {
		return false
	}
	_, imageMap := image["auto_map"]
	_, videoMap := video["auto_map"]
	return !imageMap && !videoMap && imageClass(image) && videoClass(video)
}

func imageClass(config map[string]json.RawMessage) bool {
	var class string
	return json.Unmarshal(config["image_processor_type"], &class) == nil && slices.Contains(imageProcessorClasses, class)
}

func videoClass(config map[string]json.RawMessage) bool {
	return stringIs(config["video_processor_type"], videoProcessorClass)
}

func stringIs(raw json.RawMessage, want string) bool {
	var value string
	return json.Unmarshal(raw, &value) == nil && value == want
}
