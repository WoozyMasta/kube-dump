// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package profile

// omitEmptyFields removes explicitly empty object fields in place.
//
// Null values and empty maps or lists are omitted from maps.
// Scalar zero values and empty strings are preserved because they can carry configuration meaning.
// Values inside lists remain list items even when their contents are empty.
func omitEmptyFields(object map[string]any) {
	for key, value := range object {
		cleaned, keep := compactValue(value, false)
		if !keep {
			delete(object, key)
			continue
		}

		object[key] = cleaned
	}
}

// compactValue removes empty descendants and reports whether a map field should remain in its parent.
// The root object is handled separately and is retained.
func compactValue(value any, listItem bool) (any, bool) {
	switch value := value.(type) {
	case nil:
		return value, listItem

	case map[string]any:
		for key, child := range value {
			cleaned, keep := compactValue(child, false)
			if !keep {
				delete(value, key)
				continue
			}

			value[key] = cleaned
		}
		return value, len(value) != 0 || listItem

	case []any:
		for index, item := range value {
			cleaned, _ := compactValue(item, true)
			value[index] = cleaned
		}
		return value, len(value) != 0 || listItem

	default:
		return value, true
	}
}
