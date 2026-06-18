package filesystem

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestReadFileWithOffsetAndLimit(t *testing.T) {
	tool := NewReadFileTool(".")

	tests := []struct {
		name    string
		input   readFileInput
		wantErr bool
		check   func(t *testing.T, content string)
	}{
		{
			name: "read full file",
			input: readFileInput{
				Path: "read_file.go",
			},
			check: func(t *testing.T, content string) {
				if content == "" {
					t.Error("expected non-empty content")
				}
				if !strings.HasPrefix(content, "package filesystem") {
					t.Errorf("expected to start with 'package filesystem', got %q", content[:20])
				}
			},
		},
		{
			name: "offset line 2 skips first line",
			input: readFileInput{
				Path:   "read_file.go",
				Offset: 2,
			},
			check: func(t *testing.T, content string) {
				if strings.HasPrefix(content, "package filesystem") {
					t.Error("expected offset 2 to skip the package line")
				}
			},
		},
		{
			name: "limit reads first N lines",
			input: readFileInput{
				Path:  "read_file.go",
				Limit: 3,
			},
			check: func(t *testing.T, content string) {
				lines := strings.Split(content, "\n")
				if len(lines) > 3 {
					t.Errorf("expected at most 3 lines, got %d", len(lines))
				}
			},
		},
		{
			name: "offset and limit together",
			input: readFileInput{
				Path:   "read_file.go",
				Offset: 5,
				Limit:  2,
			},
			check: func(t *testing.T, content string) {
				lines := strings.Split(content, "\n")
				if len(lines) > 2 {
					t.Errorf("expected at most 2 lines, got %d", len(lines))
				}
			},
		},
		{
			name: "offset beyond file returns empty",
			input: readFileInput{
				Path:   "read_file.go",
				Offset: 999999,
			},
			check: func(t *testing.T, content string) {
				if content != "" {
					t.Errorf("expected empty content when offset beyond file, got %q", content)
				}
			},
		},
		{
			name: "zero offset reads from line 1",
			input: readFileInput{
				Path:   "read_file.go",
				Offset: 0,
				Limit:  1,
			},
			check: func(t *testing.T, content string) {
				if !strings.HasPrefix(content, "package filesystem") {
					t.Errorf("expected first line, got %q", content)
				}
			},
		},
		{
			name: "negative offset treated as 0",
			input: readFileInput{
				Path:   "read_file.go",
				Offset: -5,
				Limit:  1,
			},
			check: func(t *testing.T, content string) {
				if !strings.HasPrefix(content, "package filesystem") {
					t.Errorf("expected first line, got %q", content)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input, _ := json.Marshal(tt.input)
			result, err := tool.Execute(context.Background(), input)

			if (err != nil) != tt.wantErr {
				t.Errorf("Execute() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.check != nil {
				tt.check(t, result.Content)
			}
		})
	}
}

func TestReadFileStringNumberInput(t *testing.T) {
	tool := NewReadFileTool(".")

	// Models often pass numeric fields as JSON strings: "offset": "5"
	raw := json.RawMessage(`{"path": "read_file.go", "offset": "5", "limit": "2"}`)
	result, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("Execute() with string numbers error = %v", err)
	}
	lines := strings.Split(result.Content, "\n")
	if len(lines) > 2 {
		t.Errorf("expected at most 2 lines with string-number limit, got %d", len(lines))
	}
}

func TestReadFileMixedNumberTypes(t *testing.T) {
	tool := NewReadFileTool(".")

	// Mixed: offset as number, limit as string
	raw := json.RawMessage(`{"path": "read_file.go", "offset": 3, "limit": "1"}`)
	result, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("Execute() with mixed types error = %v", err)
	}
	lines := strings.Split(result.Content, "\n")
	if len(lines) != 1 {
		t.Errorf("expected exactly 1 line, got %d lines", len(lines))
	}
}

func TestReadFileInvalidStringNumber(t *testing.T) {
	tool := NewReadFileTool(".")

	raw := json.RawMessage(`{"path": "read_file.go", "offset": "abc"}`)
	_, err := tool.Execute(context.Background(), raw)
	if err == nil {
		t.Fatal("expected error for invalid string number")
	}
}

func TestReadFileNoOffsetLimit(t *testing.T) {
	tool := NewReadFileTool(".")

	// Just path, no offset/limit — reads the full file
	raw := json.RawMessage(`{"path": "read_file.go"}`)
	result, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.HasPrefix(result.Content, "package filesystem") {
		t.Errorf("expected full file to start with 'package filesystem', got %q", result.Content[:20])
	}
}