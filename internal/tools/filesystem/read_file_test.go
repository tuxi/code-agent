package filesystem

import (
	"context"
	"encoding/json"
	"testing"
)

func TestReadFileWithOffsetAndLimit(t *testing.T) {
	tool := NewReadFileTool(".")

	tests := []struct {
		name     string
		input    readFileInput
		want     string
		wantErr  bool
	}{
		{
			name: "read full file",
			input: readFileInput{
				Path: "read_file.go",
			},
			wantErr: false,
		},
		{
			name: "read with offset",
			input: readFileInput{
				Path:   "read_file.go",
				Offset: 10,
			},
			wantErr: false,
		},
		{
			name: "read with limit",
			input: readFileInput{
				Path:  "read_file.go",
				Limit: 100,
			},
			wantErr: false,
		},
		{
			name: "read with offset and limit",
			input: readFileInput{
				Path:   "read_file.go",
				Offset: 50,
				Limit:  100,
			},
			wantErr: false,
		},
		{
			name: "offset beyond file size",
			input: readFileInput{
				Path:   "read_file.go",
				Offset: 999999,
			},
			want:    "",
			wantErr: false,
		},
		{
			name: "zero offset and limit",
			input: readFileInput{
				Path:   "read_file.go",
				Offset: 0,
				Limit:  0,
			},
			wantErr: false,
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

			if tt.want != "" && result.Content != tt.want {
				t.Errorf("Execute() content = %q, want %q", result.Content, tt.want)
			}

			if tt.want == "" && tt.wantErr == false && tt.input.Offset == 999999 {
				if result.Content != "" {
					t.Errorf("Execute() content should be empty when offset beyond file size, got %q", result.Content)
				}
			}

			// Verify offset is applied
			if tt.input.Offset > 0 && tt.input.Offset < 999999 {
				// Read full file to compare
				fullInput, _ := json.Marshal(readFileInput{Path: tt.input.Path})
				fullResult, _ := tool.Execute(nil, fullInput)
				if len(fullResult.Content) > tt.input.Offset {
					expected := fullResult.Content[tt.input.Offset:]
					if tt.input.Limit > 0 && len(expected) > tt.input.Limit {
						expected = expected[:tt.input.Limit]
					}
					if result.Content != expected {
						t.Errorf("Offset/Limit not applied correctly. Got %q, want %q", result.Content, expected)
					}
				}
			}
		})
	}
}