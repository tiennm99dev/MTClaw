package tools

// objectSchema builds a JSON Schema object description for one tool's
// arguments: properties plus which of them are required. additionalProperties
// is always false so the model cannot smuggle extra, silently-ignored keys
// into a call.
func objectSchema(properties map[string]any, required ...string) map[string]any {
	req := required
	if req == nil {
		req = []string{}
	}
	return map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             req,
		"additionalProperties": false,
	}
}

func stringProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func integerProp(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description}
}

func enumProp(description string, values ...string) map[string]any {
	return map[string]any{"type": "string", "description": description, "enum": values}
}
