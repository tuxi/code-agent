package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"code-agent/internal/runtime"
	"code-agent/internal/session"
)

const maxCharsPerFile = 100_000

// runTranscripts handles `codeagent transcripts [--since <days>] [--output <dir>]`.
func runTranscripts(args []string) error {
	var sinceDays int
	var outputDir string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--since":
			if i+1 < len(args) {
				d, err := strconv.Atoi(args[i+1])
				if err != nil {
					return fmt.Errorf("invalid --since value: %s", args[i+1])
				}
				sinceDays = d
				i++
			}
		case "--output":
			if i+1 < len(args) {
				outputDir = args[i+1]
				i++
			}
		default:
			return fmt.Errorf("unexpected argument: %s", args[i])
		}
	}

	if outputDir == "" {
		outputDir = "./session-transcripts"
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return err
	}

	root, _ := os.Getwd()
	store, err := runtime.OpenStore(root)
	if err != nil {
		return err
	}
	defer store.Close()

	ctx := context.Background()

	metas, err := store.List(ctx)
	if err != nil {
		return err
	}

	var transcripts []transcript
	for _, m := range metas {
		if sinceDays > 0 {
			cutoff := time.Now().AddDate(0, 0, -sinceDays)
			if m.UpdatedAt.Before(cutoff) {
				continue
			}
		}
		// Best-effort: skip sessions whose events can't be read.
		msgs, err := extractSessionMessages(ctx, store, m.ID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "transcripts: skip %s: %v\n", m.ID, err)
			continue
		}
		if len(msgs) > 0 {
			transcripts = append(transcripts, transcript{SessionID: m.ID, Messages: msgs})
		}
	}
	sort.Slice(transcripts, func(i, j int) bool { return transcripts[i].SessionID < transcripts[j].SessionID })
	fmt.Printf("Found %d session(s)\n", len(transcripts))

	if len(transcripts) == 0 {
		fmt.Println("No transcripts to export.")
		return nil
	}

	outputFiles := writeTranscriptChunks(transcripts, outputDir)
	fmt.Printf("Wrote %d transcript file(s) to %s\n", len(outputFiles), outputDir)
	return nil
}

type transcript struct {
	SessionID string
	Messages  []string
}

func extractSessionMessages(ctx context.Context, store session.Store, sessionID string) ([]string, error) {
	events, err := store.SessionEvents(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	var msgs []string
	for _, ev := range events {
		var raw map[string]interface{}
		if err := json.Unmarshal(ev.Payload, &raw); err != nil {
			continue
		}
		kind, _ := raw["Kind"].(string)
		text, _ := raw["Text"].(string)
		switch kind {
		case "turn_finished", "thinking", "reflected", "verified":
			text = strings.TrimSpace(text)
			if text == "" || strings.HasPrefix(text, "[observation]") {
				continue
			}
			msgs = append(msgs, fmt.Sprintf("[ASSISTANT]\n%s", text))
		}
	}
	return msgs, nil
}

func writeTranscriptChunks(transcripts []transcript, outputDir string) []string {
	var files []string
	var current strings.Builder
	fileIndex := 0

	writeCurrent := func() {
		if current.Len() == 0 {
			return
		}
		name := fmt.Sprintf("session-transcripts-%03d.txt", fileIndex)
		if err := os.WriteFile(filepath.Join(outputDir, name), []byte(current.String()), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "transcripts: write %s: %v\n", name, err)
			return
		}
		files = append(files, name)
		fmt.Printf("  Wrote %s (%d chars)\n", name, current.Len())
		current.Reset()
		fileIndex++
	}

	for _, t := range transcripts {
		block := fmt.Sprintf("=== SESSION: %s ===\n%s\n=== END SESSION ===\n",
			t.SessionID, strings.Join(t.Messages, "\n---\n"))

		if current.Len() > 0 && current.Len()+len(block)+2 > maxCharsPerFile {
			writeCurrent()
		}
		if len(block) > maxCharsPerFile {
			writeCurrent()
			name := fmt.Sprintf("session-transcripts-%03d.txt", fileIndex)
			if err := os.WriteFile(filepath.Join(outputDir, name), []byte(block), 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "transcripts: write %s: %v\n", name, err)
				continue
			}
			files = append(files, name)
			fmt.Printf("  Wrote %s (%d chars, oversized)\n", name, len(block))
			fileIndex++
			continue
		}
		current.WriteString(block)
		if current.Len() > 0 && !strings.HasSuffix(current.String(), "\n") {
			current.WriteString("\n")
		}
	}
	writeCurrent()
	return files
}
