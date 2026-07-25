package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

type protocolPatchAction string

const (
	patchActionAdd     protocolPatchAction = "add_file"
	patchActionDelete  protocolPatchAction = "delete_file"
	patchActionUpdate  protocolPatchAction = "update_file"
	patchActionReplace protocolPatchAction = "replace_file"
	patchActionBatch   protocolPatchAction = "batch"
)

type protocolCustomToolSpec struct {
	OpenAIName string
	ApplyPatch bool
	Action     protocolPatchAction
}

type protocolFunctionToolSpec struct {
	Namespace string
	Name      string
}

type protocolToolContext struct {
	Custom   map[string]protocolCustomToolSpec
	Function map[string]protocolFunctionToolSpec
}

func buildProtocolToolContext(raw any) protocolToolContext {
	ctx := protocolToolContext{Custom: map[string]protocolCustomToolSpec{}, Function: map[string]protocolFunctionToolSpec{}}
	tools, _ := raw.([]any)
	for _, rawTool := range tools {
		if name, ok := rawTool.(string); ok && strings.TrimSpace(name) != "" {
			name = strings.TrimSpace(name)
			if action, ok := patchActionFromProxyName(name); ok {
				ctx.Custom[name] = protocolCustomToolSpec{OpenAIName: "apply_patch", ApplyPatch: true, Action: action}
			} else {
				ctx.Custom[name] = protocolCustomToolSpec{OpenAIName: name}
			}
			continue
		}
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		typeName := strings.TrimSpace(stringFromAny(tool["type"]))
		name := strings.TrimSpace(stringFromAny(tool["name"]))
		switch typeName {
		case "custom":
			if name == "" {
				continue
			}
			isPatch := isApplyPatchTool(tool, name)
			ctx.Custom[name] = protocolCustomToolSpec{OpenAIName: name, ApplyPatch: isPatch}
			if isPatch {
				for _, action := range []protocolPatchAction{patchActionAdd, patchActionDelete, patchActionUpdate, patchActionReplace, patchActionBatch} {
					ctx.Custom[name+"_"+string(action)] = protocolCustomToolSpec{OpenAIName: name, ApplyPatch: true, Action: action}
				}
			}
		case "function":
			if name != "" {
				ctx.Function[name] = protocolFunctionToolSpec{Name: name}
			}
		case "namespace":
			children, _ := tool["tools"].([]any)
			for _, rawChild := range children {
				child, _ := rawChild.(map[string]any)
				if stringFromAny(child["type"]) != "function" {
					continue
				}
				childName := strings.TrimSpace(stringFromAny(child["name"]))
				if childName == "" {
					continue
				}
				flat := flattenProtocolToolName(name, childName)
				if existing, found := ctx.Function[flat]; !found || existing.Namespace != "" {
					ctx.Function[flat] = protocolFunctionToolSpec{Namespace: name, Name: childName}
				}
			}
		case "web_search", "local_shell", "computer_use":
			if name == "" {
				name = typeName
			}
			ctx.Custom[name] = protocolCustomToolSpec{OpenAIName: name}
		}
	}
	return ctx
}

func convertResponsesToolsToChat(raw any, ctx protocolToolContext) []any {
	items, _ := raw.([]any)
	tools := make([]any, 0, len(items))
	for index, rawTool := range items {
		if name, ok := rawTool.(string); ok && strings.TrimSpace(name) != "" {
			tools = append(tools, genericCustomProxyTool(strings.TrimSpace(name), ""))
			continue
		}
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		typeName := stringFromAny(tool["type"])
		name := strings.TrimSpace(stringFromAny(tool["name"]))
		switch typeName {
		case "function":
			if converted := responseFunctionToolToChat(tool, index); converted != nil {
				tools = append(tools, converted)
			}
		case "custom", "web_search", "local_shell", "computer_use":
			if name == "" {
				name = typeName
			}
			description := stringFromAny(tool["description"])
			if isApplyPatchTool(tool, name) {
				tools = append(tools, applyPatchProxyTools(name, description)...)
			} else {
				tools = append(tools, genericCustomProxyTool(name, description))
			}
		case "namespace":
			children, _ := tool["tools"].([]any)
			for _, rawChild := range children {
				child, _ := rawChild.(map[string]any)
				if stringFromAny(child["type"]) != "function" {
					continue
				}
				childName := strings.TrimSpace(stringFromAny(child["name"]))
				flat := flattenProtocolToolName(name, childName)
				if childName == "" || (name != "" && ctx.Function[flat].Namespace == "") {
					continue
				}
				description := strings.TrimSpace(strings.Join(nonEmptyStrings(stringFromAny(tool["description"]), stringFromAny(child["description"])), "\n\n"))
				function := map[string]any{"name": flat, "parameters": normalizeChatToolParameters(child["parameters"])}
				if description != "" {
					function["description"] = description
				}
				tools = append(tools, map[string]any{"type": "function", "function": function})
			}
		}
	}
	return tools
}

func responseFunctionToolToChat(tool map[string]any, index int) any {
	if nested, ok := tool["function"].(map[string]any); ok {
		function := cloneMap(nested)
		function["parameters"] = normalizeChatToolParameters(function["parameters"])
		if function["strict"] == nil && tool["strict"] != nil {
			function["strict"] = tool["strict"]
		}
		return map[string]any{"type": "function", "function": function}
	}
	name := strings.TrimSpace(stringFromAny(tool["name"]))
	if name == "" {
		name = fmt.Sprintf("tool_%d", index)
	}
	function := map[string]any{
		"name":        name,
		"description": stringFromAny(tool["description"]),
		"parameters":  normalizeChatToolParameters(tool["parameters"]),
	}
	if tool["strict"] != nil {
		function["strict"] = tool["strict"]
	}
	return map[string]any{"type": "function", "function": function}
}

func normalizeChatToolParameters(raw any) map[string]any {
	parameters, _ := raw.(map[string]any)
	parameters = cloneMap(parameters)
	if parameters["type"] == nil {
		parameters["type"] = "object"
	}
	if parameters["properties"] == nil {
		parameters["properties"] = map[string]any{}
	}
	if parameters["required"] == nil {
		parameters["required"] = []any{}
	}
	return parameters
}

func convertResponsesToolChoice(raw any, ctx protocolToolContext) any {
	switch choice := raw.(type) {
	case string:
		if choice == "auto" || choice == "none" || choice == "required" {
			return choice
		}
	case map[string]any:
		typeName := stringFromAny(choice["type"])
		if typeName == "custom" {
			name := stringFromAny(choice["name"])
			spec, ok := ctx.Custom[name]
			if !ok {
				return nil
			}
			if spec.ApplyPatch {
				name = spec.OpenAIName + "_batch"
			}
			return map[string]any{"type": "function", "function": map[string]any{"name": name}}
		}
		function, _ := choice["function"].(map[string]any)
		name := firstNonEmpty(stringFromAny(choice["name"]), stringFromAny(function["name"]))
		namespace := firstNonEmpty(stringFromAny(choice["namespace"]), stringFromAny(function["namespace"]))
		if name != "" {
			return map[string]any{"type": "function", "function": map[string]any{"name": flattenProtocolToolName(namespace, name)}}
		}
	}
	return nil
}

func genericCustomProxyTool(name, description string) map[string]any {
	description = strings.TrimSpace(description)
	if description == "" {
		description = "FREEFORM custom tool: " + name + ". Put only the tool input text here."
	} else {
		description += "\n\nThis is a FREEFORM tool. Do not wrap the input in JSON or markdown."
	}
	return protocolFunctionTool(name, description, map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{"input": map[string]any{"type": "string", "description": "Raw freeform input for this custom tool."}},
		"required":   []any{"input"},
	})
}

func applyPatchProxyTools(name, description string) []any {
	desc := func(action, fallback string) string {
		if strings.TrimSpace(description) == "" {
			return fallback
		}
		return strings.TrimSpace(description) + " (proxy action: " + action + ")"
	}
	return []any{
		protocolFunctionTool(name+"_add_file", desc("add_file", "Create one new file by providing a target path and full file content."), patchAddSchema()),
		protocolFunctionTool(name+"_delete_file", desc("delete_file", "Delete one file by providing a target path."), patchDeleteSchema()),
		protocolFunctionTool(name+"_update_file", desc("update_file", "Edit one existing file with structured hunks."), patchUpdateSchema()),
		protocolFunctionTool(name+"_replace_file", desc("replace_file", "Replace one existing file by providing a target path and full new file content."), patchReplaceSchema()),
		protocolFunctionTool(name+"_batch", desc("batch", "Edit files by providing structured JSON patch operations."), patchBatchSchema()),
	}
}

func protocolFunctionTool(name, description string, parameters map[string]any) map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{"name": name, "description": description, "parameters": parameters}}
}

func patchAddSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"path": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}}, "required": []any{"path", "content"}}
}

func patchDeleteSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []any{"path"}}
}

func patchHunksSchema() map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"context": map[string]any{"type": "string"}, "lines": map[string]any{"type": "array", "items": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"op": map[string]any{"type": "string", "enum": []any{"context", "add", "remove"}}, "text": map[string]any{"type": "string"}}, "required": []any{"op", "text"}}}}, "required": []any{"lines"}}}
}

func patchUpdateSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"path": map[string]any{"type": "string"}, "move_to": map[string]any{"type": "string"}, "hunks": patchHunksSchema()}, "required": []any{"path", "hunks"}}
}

func patchReplaceSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"path": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}}, "required": []any{"path", "content"}}
}

func patchBatchSchema() map[string]any {
	operation := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"type": map[string]any{"type": "string", "enum": []any{"add_file", "delete_file", "update_file", "replace_file"}}, "path": map[string]any{"type": "string"}, "move_to": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}, "hunks": patchHunksSchema()}, "required": []any{"type", "path"}}
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"operations": map[string]any{"type": "array", "items": operation}}, "required": []any{"operations"}}
}

func isApplyPatchTool(tool map[string]any, name string) bool {
	if name == "apply_patch" {
		return true
	}
	format, _ := tool["format"].(map[string]any)
	definition := stringFromAny(format["definition"])
	return strings.Contains(definition, "begin_patch") && strings.Contains(definition, "end_patch") && strings.Contains(definition, "add_hunk")
}

func patchActionFromProxyName(name string) (protocolPatchAction, bool) {
	for _, action := range []protocolPatchAction{patchActionAdd, patchActionDelete, patchActionUpdate, patchActionReplace, patchActionBatch} {
		if strings.HasSuffix(name, "_"+string(action)) {
			return action, true
		}
	}
	return "", false
}

func flattenProtocolToolName(namespace, name string) string {
	if namespace == "" {
		return name
	}
	if name == "" {
		return namespace
	}
	if strings.HasSuffix(namespace, "__") || strings.HasPrefix(name, "__") {
		return namespace + name
	}
	return namespace + "__" + name
}

func protocolResponseToolCall(callID, name, arguments string, ctx protocolToolContext) map[string]any {
	if spec, ok := ctx.Custom[name]; ok {
		input := reconstructCustomToolInput(arguments)
		if spec.ApplyPatch {
			input = reconstructApplyPatchInput(spec.Action, arguments)
		}
		return map[string]any{"id": "ctc_" + callID, "type": "custom_tool_call", "status": "completed", "call_id": callID, "name": spec.OpenAIName, "input": input}
	}
	displayName, namespace := name, ""
	if spec, ok := ctx.Function[name]; ok {
		if spec.Name != "" {
			displayName = spec.Name
		}
		namespace = spec.Namespace
	}
	item := map[string]any{"id": "fc_" + callID, "type": "function_call", "status": "completed", "call_id": callID, "name": displayName, "arguments": normalizeChatToolArguments(arguments)}
	if namespace != "" {
		item["namespace"] = namespace
	}
	return item
}

func normalizeChatToolArguments(raw any) string {
	if raw == nil {
		return "{}"
	}
	if text, ok := raw.(string); ok {
		var value any
		if json.Unmarshal([]byte(text), &value) == nil {
			if _, object := value.(map[string]any); object {
				return canonicalJSONString(value)
			}
			return canonicalJSONString(map[string]any{"input": value})
		}
		return canonicalJSONString(map[string]any{"input": text})
	}
	if _, object := raw.(map[string]any); object {
		return canonicalJSONString(raw)
	}
	return canonicalJSONString(map[string]any{"input": raw})
}

func buildCustomToolHistory(name string, input any) (string, string) {
	text := responseText(input)
	if name != "apply_patch" && !strings.HasPrefix(text, "*** Begin Patch") {
		return name, canonicalJSONString(map[string]any{"input": text})
	}
	operations := parseApplyPatchOperations(text)
	if len(operations) == 1 {
		action := protocolPatchAction(stringFromAny(operations[0]["type"]))
		if action != patchActionAdd && action != patchActionDelete && action != patchActionUpdate && action != patchActionReplace {
			action = patchActionBatch
		}
		return name + "_" + string(action), patchOperationArguments(operations[0], action)
	}
	return name + "_batch", canonicalJSONString(map[string]any{"operations": operations, "raw_patch": text})
}

func reconstructCustomToolInput(arguments string) string {
	var value map[string]any
	if json.Unmarshal([]byte(arguments), &value) != nil || value["input"] == nil {
		return arguments
	}
	return responseText(value["input"])
}

func reconstructApplyPatchInput(action protocolPatchAction, arguments string) string {
	var value map[string]any
	if json.Unmarshal([]byte(arguments), &value) != nil {
		return arguments
	}
	for _, key := range []string{"raw_patch", "patch", "input"} {
		if text := stringFromAny(value[key]); text != "" {
			return text
		}
	}
	var operations []map[string]any
	switch action {
	case patchActionAdd, patchActionDelete, patchActionUpdate, patchActionReplace:
		op := cloneMap(value)
		op["type"] = string(action)
		operations = append(operations, op)
	default:
		for _, raw := range anySlice(value["operations"]) {
			if operation, ok := raw.(map[string]any); ok {
				operations = append(operations, operation)
			}
		}
	}
	return buildApplyPatchText(operations)
}

func buildApplyPatchText(operations []map[string]any) string {
	var out strings.Builder
	out.WriteString("*** Begin Patch")
	for _, operation := range operations {
		path := stringFromAny(operation["path"])
		switch stringFromAny(operation["type"]) {
		case "add_file":
			out.WriteString("\n*** Add File: " + path)
			for _, line := range strings.Split(strings.TrimSuffix(stringFromAny(operation["content"]), "\n"), "\n") {
				if line != "" || stringFromAny(operation["content"]) != "" {
					out.WriteString("\n+" + line)
				}
			}
		case "delete_file":
			out.WriteString("\n*** Delete File: " + path)
		case "replace_file":
			out.WriteString("\n*** Delete File: " + path + "\n*** Add File: " + path)
			for _, line := range strings.Split(strings.TrimSuffix(stringFromAny(operation["content"]), "\n"), "\n") {
				if line != "" || stringFromAny(operation["content"]) != "" {
					out.WriteString("\n+" + line)
				}
			}
		case "update_file":
			out.WriteString("\n*** Update File: " + path)
			if moveTo := stringFromAny(operation["move_to"]); moveTo != "" {
				out.WriteString("\n*** Move to: " + moveTo)
			}
			for _, rawHunk := range anySlice(operation["hunks"]) {
				hunk, _ := rawHunk.(map[string]any)
				out.WriteString("\n@@")
				if context := stringFromAny(hunk["context"]); context != "" {
					out.WriteString(" " + context)
				}
				for _, rawLine := range anySlice(hunk["lines"]) {
					line, _ := rawLine.(map[string]any)
					prefix := " "
					if stringFromAny(line["op"]) == "add" {
						prefix = "+"
					} else if op := stringFromAny(line["op"]); op == "remove" || op == "delete" {
						prefix = "-"
					}
					out.WriteString("\n" + prefix + stringFromAny(line["text"]))
				}
			}
		}
	}
	out.WriteString("\n*** End Patch")
	return out.String()
}

func parseApplyPatchOperations(input string) []map[string]any {
	var operations []map[string]any
	var current map[string]any
	var content []string
	var hunks []any
	var hunk map[string]any
	var lines []any
	flushHunk := func() {
		if hunk != nil {
			hunk["lines"] = lines
			hunks = append(hunks, hunk)
			hunk, lines = nil, nil
		}
	}
	flushOperation := func() {
		if current == nil {
			return
		}
		if typeName := stringFromAny(current["type"]); typeName == "add_file" || typeName == "replace_file" {
			current["content"] = strings.Join(content, "\n")
		} else if typeName == "update_file" {
			current["hunks"] = hunks
		}
		operations = append(operations, current)
		current, content, hunks = nil, nil, nil
	}
	for _, line := range strings.Split(strings.ReplaceAll(input, "\r\n", "\n"), "\n") {
		start := func(typeName, prefix string) bool {
			if !strings.HasPrefix(line, prefix) {
				return false
			}
			flushHunk()
			flushOperation()
			current = map[string]any{"type": typeName, "path": strings.TrimPrefix(line, prefix)}
			return true
		}
		if line == "*** Begin Patch" || line == "*** End Patch" || start("add_file", "*** Add File: ") || start("delete_file", "*** Delete File: ") || start("update_file", "*** Update File: ") {
			continue
		}
		if strings.HasPrefix(line, "*** Move to: ") && current != nil {
			current["move_to"] = strings.TrimPrefix(line, "*** Move to: ")
			continue
		}
		if strings.HasPrefix(line, "@@") {
			flushHunk()
			hunk = map[string]any{"context": strings.TrimSpace(strings.TrimPrefix(line, "@@"))}
			continue
		}
		if current == nil {
			continue
		}
		switch stringFromAny(current["type"]) {
		case "add_file", "replace_file":
			if strings.HasPrefix(line, "+") {
				content = append(content, strings.TrimPrefix(line, "+"))
			}
		case "update_file":
			op, text := "context", line
			if strings.HasPrefix(line, "+") {
				op, text = "add", line[1:]
			} else if strings.HasPrefix(line, "-") {
				op, text = "remove", line[1:]
			} else if strings.HasPrefix(line, " ") {
				text = line[1:]
			}
			lines = append(lines, map[string]any{"op": op, "text": text})
		}
	}
	flushHunk()
	flushOperation()
	return operations
}

func patchOperationArguments(operation map[string]any, action protocolPatchAction) string {
	result := map[string]any{"path": stringFromAny(operation["path"])}
	switch action {
	case patchActionAdd, patchActionReplace:
		result["content"] = stringFromAny(operation["content"])
	case patchActionUpdate:
		result["hunks"] = firstNonNil(operation["hunks"], []any{})
		if moveTo := stringFromAny(operation["move_to"]); moveTo != "" {
			result["move_to"] = moveTo
		}
	case patchActionBatch:
		result = map[string]any{"operations": []any{operation}}
	}
	return canonicalJSONString(result)
}

func anySlice(value any) []any {
	items, _ := value.([]any)
	return items
}

func nonEmptyStrings(values ...string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			result = append(result, strings.TrimSpace(value))
		}
	}
	return result
}
