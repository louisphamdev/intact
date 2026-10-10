package servertools

import (
	"encoding/json"
	"strings"

	"github.com/louisphamdev/intact/internal/websearch"
)

// FillClientTools gives an Anthropic-defined client tool the schema Anthropic
// keeps on its side. A request declares bash, the text editor or the memory tool
// by type and name alone ({"type":"bash_20250124","name":"bash"}) because Claude
// was trained on their input; any other model needs the schema to call them. The
// client still runs them. It returns the body with the schemas added, and the
// tools that are still without one: a provider that is not Anthropic cannot take
// those, so the request goes without them.
func FillClientTools(body []byte) ([]byte, []string) {
	m, err := decode(body)
	if err != nil {
		return body, nil
	}
	tools := list(m["tools"])
	var dropped []string
	changed := false
	for _, t := range tools {
		td := asObj(t)
		if td == nil || td["input_schema"] != nil {
			continue
		}
		typ := str(td["type"])
		if desc, schema := fixedSchema(typ); schema != nil {
			td["input_schema"] = schema
			if str(td["description"]) == "" {
				td["description"] = desc
			}
			changed = true
			continue
		}
		if intactRuns(t, typ) {
			continue
		}
		dropped = append(dropped, describeTool(td, typ))
	}
	if !changed {
		return body, dropped
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body, dropped
	}
	return out, dropped
}

// intactRuns reports whether intact answers for the tool itself.
func intactRuns(t any, typ string) bool {
	return websearch.IsHostedTool(t) || strings.HasPrefix(typ, "web_fetch") ||
		strings.HasPrefix(typ, "tool_search_tool_") || typ == "mcp_toolset"
}

func describeTool(td obj, typ string) string {
	name := str(td["name"])
	if typ == "" {
		return name
	}
	return typ + " (" + name + ")"
}

// fixedSchema is the description and input schema of an Anthropic-defined
// client tool, by its type, or nil for a type that is not one.
func fixedSchema(typ string) (string, obj) {
	switch {
	case strings.HasPrefix(typ, "bash_"):
		return "Run commands in a persistent bash session. Set restart to true to restart the session.",
			schema(obj{
				"command": obj{"type": "string", "description": "The bash command to run. Required unless restart is true."},
				"restart": obj{"type": "boolean", "description": "Set to true to restart the bash session."},
			})
	case strings.HasPrefix(typ, "text_editor_"):
		commands := []any{"view", "create", "str_replace", "insert"}
		if typ == "text_editor_20250124" || typ == "text_editor_20241022" {
			commands = append(commands, "undo_edit")
		}
		return "View and edit text files. Commands: view (a file or a directory), create, str_replace, insert.",
			schema(obj{
				"command":     obj{"type": "string", "enum": commands, "description": "The command to run."},
				"path":        obj{"type": "string", "description": "Path of the file or directory."},
				"file_text":   obj{"type": "string", "description": "Content of the file, for create."},
				"old_str":     obj{"type": "string", "description": "Text to replace, for str_replace. It must match exactly once."},
				"new_str":     obj{"type": "string", "description": "Replacement text, for str_replace."},
				"insert_line": obj{"type": "integer", "description": "Line after which to insert, for insert. 0 inserts at the start."},
				"insert_text": obj{"type": "string", "description": "Text to insert, for insert."},
				"view_range": obj{"type": "array", "items": obj{"type": "integer"},
					"description": "Start and end line to view, 1-indexed. An end of -1 reads to the end of the file."},
			}, "command", "path")
	case strings.HasPrefix(typ, "memory_"):
		return "Store and retrieve information across conversations in files under /memories. " +
				"Commands: view, create, str_replace, insert, delete, rename.",
			schema(obj{
				"command":     obj{"type": "string", "enum": []any{"view", "create", "str_replace", "insert", "delete", "rename"}, "description": "The command to run."},
				"path":        obj{"type": "string", "description": "Path under /memories."},
				"file_text":   obj{"type": "string", "description": "Content of the file, for create."},
				"old_str":     obj{"type": "string", "description": "Text to replace, for str_replace."},
				"new_str":     obj{"type": "string", "description": "Replacement text, for str_replace."},
				"insert_line": obj{"type": "integer", "description": "Line after which to insert, for insert. 0 inserts at the start."},
				"insert_text": obj{"type": "string", "description": "Text to insert, for insert."},
				"old_path":    obj{"type": "string", "description": "Current path, for rename."},
				"new_path":    obj{"type": "string", "description": "New path, for rename."},
				"view_range": obj{"type": "array", "items": obj{"type": "integer"},
					"description": "Start and end line to view, 1-indexed."},
			}, "command")
	}
	return "", nil
}

func schema(props obj, required ...string) obj {
	s := obj{"type": "object", "properties": props}
	if len(required) > 0 {
		r := make([]any, len(required))
		for i, k := range required {
			r[i] = k
		}
		s["required"] = r
	}
	return s
}
