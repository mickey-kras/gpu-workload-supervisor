package llmtraining

import (
	"encoding/json"
	"errors"
	"maps"
	"path/filepath"
	"slices"
	"strings"
)

const (
	processorClass      = "Qwen3VLProcessor"
	videoProcessorClass = "Qwen3VLVideoProcessor"

	imageProcessorName  = "preprocessor_config.json"
	videoProcessorName  = "video_preprocessor_config.json"
	processorConfigName = "processor_config.json"
)

// processorSubConfigs are the nested processor objects of a combined
// processor_config.json.
var processorSubConfigs = []string{"image_processor", "video_processor"}

// imageProcessorClasses are the classes that the pinned snapshots and the
// locked transformers name for the image processor.
var imageProcessorClasses = []string{"Qwen2VLImageProcessorFast", "Qwen2VLImageProcessor"}

// unboundLoaderNames are files and directories that the transformers
// processor loader reads but that no inventory or provenance binds: the legacy
// chat_template.json, the templates in chat_templates/ and
// additional_chat_templates/, and audio_tokenizer_config.json, which names a
// class and a path to load. The candidate and export checks share this list.
var unboundLoaderNames = []string{"chat_template.json", "chat_templates", "additional_chat_templates", "audio_tokenizer_config.json"}

// processorFileNames are the processor files, split and combined.
var processorFileNames = []string{imageProcessorName, videoProcessorName, processorConfigName}

// tokenizerSnapshotNames are the only files that the tokenizer snapshot,
// which is also the processor root, may hold.
var tokenizerSnapshotNames = slices.Concat([]string{
	tokenizerConfigName, "tokenizer.json", "tokenizer.model", "special_tokens_map.json",
	"added_tokens.json", "vocab.json", "merges.txt", "vocab.txt",
}, processorFileNames)

// unboundLoaderName returns the entry of unboundLoaderNames that path is or
// lies under, or "".
func unboundLoaderName(path string) string {
	for _, name := range unboundLoaderNames {
		if path == name || strings.HasPrefix(path, name+"/") {
			return name
		}
	}
	return ""
}

// inspectProcessors checks the processor configuration in the tokenizer
// snapshot, which training uses as the processor root. The image and video
// processor files are required; a combined processor_config.json is checked
// when present.
func inspectProcessors(inventory Inventory) error {
	// The tokenizer snapshot allowlist (inspectTokenizerSnapshotNames) also
	// rejects a model config.json, which the processor loader would read here.
	for _, path := range slices.Sorted(maps.Keys(inventory.Files)) {
		if name := unboundLoaderName(path); name != "" {
			return errors.New("candidate tokenizer snapshot must not contain " + name)
		}
	}
	for _, name := range []string{imageProcessorName, videoProcessorName} {
		if inventory.Files[name] == "" {
			return errors.New("candidate tokenizer snapshot requires " + name)
		}
	}
	for _, name := range processorFileNames {
		if inventory.Files[name] == "" {
			continue
		}
		if err := checkProcessorFile(filepath.Join(inventory.Root, name), name, "candidate", inventory.Files); err != nil {
			return err
		}
	}
	return nil
}

// checkExportProcessors requires the processor that training saved with the
// adapter, because training always loads one. It accepts a combined
// processor_config.json, or both split files, and checks every file present.
// The export checks the same bytes that it hashed: contents holds them.
func checkExportProcessors(files map[string]string, contents map[string][]byte) error {
	for _, name := range processorFileNames {
		if files[name] == "" {
			continue
		}
		if err := checkProcessorBytes(contents[name], name, "training export", files); err != nil {
			return err
		}
	}
	image, video := files[imageProcessorName] != "", files[videoProcessorName] != ""
	switch {
	case image && !video:
		return errors.New("training export has " + imageProcessorName + " without " + videoProcessorName + "; save both split files or neither")
	case video && !image:
		return errors.New("training export has " + videoProcessorName + " without " + imageProcessorName + "; save both split files or neither")
	case files[processorConfigName] == "" && !image:
		return errors.New("training export requires a complete processor: " + processorConfigName + ", or both " + imageProcessorName + " and " + videoProcessorName)
	}
	return nil
}

// checkProcessorFile requires the Qwen3VLProcessor classes and none of the
// shared loader keys, so the file selects only these processor classes of the
// locked transformers and loads no remote code. The error names the file, a
// fixed field or category and the expected value, never file content.
func checkProcessorFile(path, name, owner string, files map[string]string) error {
	config, err := readJSONObject(path, owner+" processor configuration "+name)
	if err != nil {
		return err
	}
	return checkProcessorObject(config, name, owner, files)
}

// checkProcessorBytes applies checkProcessorFile to bytes already read.
func checkProcessorBytes(raw []byte, name, owner string, files map[string]string) error {
	config, err := parseJSONObject(raw, owner+" processor configuration "+name)
	if err != nil {
		return err
	}
	return checkProcessorObject(config, name, owner, files)
}

func checkProcessorObject(config map[string]json.RawMessage, name, owner string, files map[string]string) error {
	if problem := processorProblem(config, name, files); problem != "" {
		return errors.New(owner + " processor configuration " + name + ": " + problem)
	}
	return nil
}

// processorProblem describes the first rule that config breaks, or returns ""
// when the file is accepted. It applies the shared loader key lists:
// remote-code and model-redirect keys at any depth, file-redirect and
// file-path keys at the top level and in the nested processor objects.
func processorProblem(config map[string]json.RawMessage, name string, files map[string]string) string {
	if problem := deepKeyProblem(deepLoaderKey(config, slices.Concat(RemoteCodeKeys, ModelRedirectKeys), processorSubConfigs)); problem != "" {
		return problem
	}
	if !stringIs(config["processor_class"], processorClass) {
		return "processor_class must be " + processorClass
	}
	switch name {
	case imageProcessorName:
		return imageProblem(config, topLevel, files)
	case videoProcessorName:
		return videoProblem(config, topLevel, files)
	}
	// imageProblem and videoProblem check the top level of a split file.
	if problem := fileKeysProblem(config, topLevel, files); problem != "" {
		return problem
	}
	var image, video map[string]json.RawMessage
	if json.Unmarshal(config["image_processor"], &image) != nil || image == nil {
		return "image_processor must be an object"
	}
	if json.Unmarshal(config["video_processor"], &video) != nil || video == nil {
		return "video_processor must be an object"
	}
	if problem := imageProblem(image, "image_processor", files); problem != "" {
		return problem
	}
	return videoProblem(video, "video_processor", files)
}

// deepKeyProblem describes the result of deepLoaderKey: "" when no key was
// found, an invalid configuration for an undecodable value, else the rejected
// key and its category.
func deepKeyProblem(category, key string) string {
	switch key {
	case "":
		return ""
	case undecodedValue:
		return "configuration is invalid"
	}
	return loaderKeyProblem(category, key)
}

// loaderKeyProblem describes a rejected loader key in a category.
func loaderKeyProblem(category, key string) string {
	return category + " contains " + key + ", which the loader key lists reject"
}

// fileKeysProblem applies the file-redirect and file-path lists to one object
// of a processor file. File-path keys must name files in files.
func fileKeysProblem(config map[string]json.RawMessage, category string, files map[string]string) string {
	if key := topLevelKey(config, FileRedirectKeys); key != "" {
		return loaderKeyProblem(category, key)
	}
	if !ValidFilePathValues(config, files) {
		return category + " file names must name inventoried files"
	}
	return ""
}

// imageProblem checks an image processor object. category is topLevel for
// the top level of preprocessor_config.json, else the nested object name.
func imageProblem(config map[string]json.RawMessage, category string, files map[string]string) string {
	if problem := fileKeysProblem(config, category, files); problem != "" {
		return problem
	}
	var class string
	if json.Unmarshal(config["image_processor_type"], &class) != nil || !slices.Contains(imageProcessorClasses, class) {
		return fieldName(category, "image_processor_type") + " must be " + strings.Join(imageProcessorClasses, " or ")
	}
	return ""
}

// videoProblem checks a video processor object, with the category convention
// of imageProblem.
func videoProblem(config map[string]json.RawMessage, category string, files map[string]string) string {
	if problem := fileKeysProblem(config, category, files); problem != "" {
		return problem
	}
	if !stringIs(config["video_processor_type"], videoProcessorClass) {
		return fieldName(category, "video_processor_type") + " must be " + videoProcessorClass
	}
	return ""
}

// fieldName names field inside category, or field alone at the top level.
func fieldName(category, field string) string {
	if category == topLevel {
		return field
	}
	return category + "." + field
}

// stringIs reports whether raw is the JSON string want.
func stringIs(raw json.RawMessage, want string) bool {
	var value string
	return json.Unmarshal(raw, &value) == nil && value == want
}
