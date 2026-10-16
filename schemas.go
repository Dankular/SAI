package main

// JSON schemas for the harness's own model calls. The model is made to fill a form instead of
// being asked politely for JSON, so a malformed reply can no longer drop a memory.

func jsonStr(max int) map[string]any {
	return map[string]any{"type": "string", "maxLength": max}
}

func jsonObject(required []string, props map[string]any) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}

func jsonArray(items map[string]any, min, max int) map[string]any {
	return map[string]any{"type": "array", "items": items, "minItems": min, "maxItems": max}
}

// memoryEventsSchema lists only the fields the extractor needs. Evidence is required and
// non-empty: Go discards any event whose quote it cannot find in the conversation.
func memoryEventsSchema() map[string]any {
	event := jsonObject([]string{"event", "summary", "scope", "evidence"}, map[string]any{
		"event":   map[string]any{"type": "string", "enum": []string{"solution_confirmed", "problem_resolved", "approach_failed", "note_identified", "preference_expressed", "decision_changed", "task_unfinished"}},
		"summary": jsonStr(200), "scope": jsonStr(60),
		"preference": jsonStr(200), "decision": jsonStr(200), "resolution": jsonStr(200), "information": jsonStr(300),
		"problem": jsonStr(200), "next_action": jsonStr(200),
		"evidence": jsonArray(jsonObject([]string{"message", "quote"}, map[string]any{"message": map[string]any{"type": "integer"}, "quote": jsonStr(240)}), 1, 3),
	})
	return jsonObject([]string{"events"}, map[string]any{"events": jsonArray(event, 0, 5)})
}
