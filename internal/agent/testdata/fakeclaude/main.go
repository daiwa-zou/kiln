// Command fakeclaude stands in for the claude CLI in tests.
//
// It is a real binary rather than an in-process mock so tests exercise the
// actual exec path: argv construction, working directory, environment, exit
// codes, and envelope parsing. A pure mock would validate none of that.
//
// Behavior is driven by KILN_FAKE_SCRIPT, a JSON object:
//
//	{
//	  "exit_code": 0,
//	  "stdout": "raw output, overrides the generated envelope",
//	  "stderr": "text",
//	  "sleep_ms": 0,
//	  "envelope": { ...Result fields... },
//	  "write_files": { "relative/path.md": "contents" },
//	  "write_dir": "/abs/path to write into, defaults to --add-dir",
//	  "record_args_to": "/abs/path to dump argv as JSON"
//	}
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type script struct {
	ExitCode     int               `json:"exit_code"`
	Stdout       string            `json:"stdout"`
	Stderr       string            `json:"stderr"`
	SleepMS      int               `json:"sleep_ms"`
	Envelope     map[string]any    `json:"envelope"`
	WriteFiles   map[string]string `json:"write_files"`
	WriteDir     string            `json:"write_dir"`
	RecordArgsTo string            `json:"record_args_to"`
}

func main() {
	var s script
	if raw := os.Getenv("KILN_FAKE_SCRIPT"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			fmt.Fprintf(os.Stderr, "fakeclaude: bad KILN_FAKE_SCRIPT: %v\n", err)
			os.Exit(2)
		}
	}

	args := os.Args[1:]

	if s.RecordArgsTo != "" {
		record := map[string]any{"args": args, "cwd": mustGetwd()}
		if b, err := json.MarshalIndent(record, "", "  "); err == nil {
			_ = os.WriteFile(s.RecordArgsTo, b, 0o644)
		}
	}

	if s.SleepMS > 0 {
		time.Sleep(time.Duration(s.SleepMS) * time.Millisecond)
	}

	if len(s.WriteFiles) > 0 {
		dir := s.WriteDir
		if dir == "" {
			dir = flagValue(args, "--add-dir")
		}
		for rel, content := range s.WriteFiles {
			// Deliberately not sanitized: some tests script an escape attempt
			// to prove the validator catches it.
			full := filepath.Join(dir, rel)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				fmt.Fprintf(os.Stderr, "fakeclaude: mkdir: %v\n", err)
				os.Exit(3)
			}
			if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "fakeclaude: write: %v\n", err)
				os.Exit(3)
			}
		}
	}

	if s.Stderr != "" {
		fmt.Fprint(os.Stderr, s.Stderr)
	}

	if s.Stdout != "" {
		fmt.Print(s.Stdout)
		os.Exit(s.ExitCode)
	}

	fmt.Println(string(envelopeJSON(s.Envelope, args)))
	os.Exit(s.ExitCode)
}

// envelopeJSON builds a realistic success envelope, overlaid with whatever the
// script specified, so most tests need only state the field they care about.
func envelopeJSON(overrides map[string]any, args []string) []byte {
	env := map[string]any{
		"type":               "result",
		"subtype":            "success",
		"is_error":           false,
		"stop_reason":        "end_turn",
		"terminal_reason":    "completed",
		"api_error_status":   nil,
		"session_id":         sessionID(args),
		"num_turns":          1,
		"total_cost_usd":     0.013459,
		"duration_ms":        1156,
		"result":             "ok",
		"permission_denials": []any{},
		"usage": map[string]any{
			"input_tokens":                1200,
			"output_tokens":               340,
			"cache_creation_input_tokens": 0,
			"cache_read_input_tokens":     0,
		},
	}
	for k, v := range overrides {
		env[k] = v
	}
	b, _ := json.Marshal(env)
	return b
}

func sessionID(args []string) string {
	if v := flagValue(args, "--session-id"); v != "" {
		return v
	}
	if v := flagValue(args, "--resume"); v != "" {
		return v
	}
	return "fake-session"
}

func flagValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func mustGetwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return wd
}
