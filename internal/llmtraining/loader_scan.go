package llmtraining

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
)

// topLevel is the category of a key that is not under a named nested object.
const topLevel = "top level"

// undecodedValue stands in for a key when a value cannot be decoded, so the
// scan fails closed. Callers reject it like a loader key.
const undecodedValue = "an undecodable value"

// deepLoaderKey returns the category and the first key of keys at any depth of
// object. The category is one of categories when the key is under that
// top-level key, else topLevel. Both results are fixed strings, never file
// content. Each value is decoded once, so the scan is linear in the input
// size. A value that cannot be decoded yields undecodedValue.
func deepLoaderKey(object map[string]json.RawMessage, keys []string, categories []string) (category, key string) {
	if found := topLevelKey(object, keys); found != "" {
		return topLevel, found
	}
	for _, name := range slices.Sorted(maps.Keys(object)) {
		where := topLevel
		if slices.Contains(categories, name) {
			where = name
		}
		decoder := json.NewDecoder(bytes.NewReader(object[name]))
		decoder.UseNumber()
		var value any
		if decoder.Decode(&value) != nil {
			return where, undecodedValue
		}
		if found := treeKey(value, keys); found != "" {
			return where, found
		}
	}
	return "", ""
}

// treeKey returns the first key of keys in a decoded JSON value.
func treeKey(value any, keys []string) string {
	switch v := value.(type) {
	case map[string]any:
		return objectKey(v, keys)
	case []any:
		for _, item := range v {
			if key := treeKey(item, keys); key != "" {
				return key
			}
		}
	}
	return ""
}

func objectKey(object map[string]any, keys []string) string {
	if key := topLevelKey(object, keys); key != "" {
		return key
	}
	for _, name := range slices.Sorted(maps.Keys(object)) {
		if key := treeKey(object[name], keys); key != "" {
			return key
		}
	}
	return ""
}

// topLevelKey returns the first key of keys that object holds.
func topLevelKey[V any](object map[string]V, keys []string) string {
	for _, key := range keys {
		if _, exists := object[key]; exists {
			return key
		}
	}
	return ""
}
