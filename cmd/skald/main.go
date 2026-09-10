package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"

	"github.com/algorhythmic/skald/sessioncapture"
	"github.com/algorhythmic/skald/sessionrecord"
)

const usage = `skald — conversation context across projects and environments

Source connection:
  skald sources --provider claude_code|codex [--root DIRECTORY] [--since RFC3339|all] [--limit 10]
  skald connect --provider claude_code|codex [--root DIRECTORY] [--config FILE]
                [--namespace ID] [--since RFC3339|all] [--max-sessions 64]

Archive commands:
  skald tui [--socket PATH] [--namespace ID] [--theme desktop|amber]
  skald serve [--config FILE] [--data-dir DIRECTORY] [--socket PATH]
  skald status [--socket PATH] [--namespace ID]
  skald sessions [--socket PATH] [--namespace ID] [--limit 25] [--cursor CURSOR]
  skald artifacts --conversation KEY [--socket PATH] [--namespace ID] [--limit 25] [--cursor CURSOR]
  skald get --record KEY --revision DIGEST --adapter VERSION [--socket PATH] [--namespace ID] [--raw]
  skald backup [--socket PATH]
  skald restore --backup FILE --data-dir NEW_DIRECTORY

Foundation commands (read-only):
  skald inspect --provider claude_code|codex --namespace ID --stream-id ID --file FILE
                [--conversation-id ID] [--checkpoint FILE] [--limit 256] [--raw]
  skald probe --provider claude_code|codex --provider-version VERSION
  skald discover --root DIRECTORY [--limit 1000]
  skald validate --file RECORD.json
  skald version

inspect emits a bounded batch and a candidate checkpoint. Persist that checkpoint
only together with the corresponding records and gaps. Original bytes are emitted
as base64 only with --raw. No default provider directories are scanned.
`

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		_ = writeJSON(os.Stderr, map[string]string{"error": err.Error()})
		os.Exit(1)
	}
}

func run(args []string, out, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		_, err := io.WriteString(out, usage)
		return err
	}
	if args[0] == "version" {
		if len(args) != 1 {
			return errors.New("unexpected_arguments")
		}
		_, err := fmt.Fprintln(out, "skald 0.1.0-dev · sessionrecord v1 · archive schema 2")
		return err
	}
	if args[0] == "sources" || args[0] == "connect" {
		return runConnect(args, out, stderr)
	}
	if args[0] == "tui" {
		return runTUI(args[1:], stderr)
	}
	if isArchiveCommand(args[0]) {
		return runArchive(args, out, stderr)
	}
	f := flag.NewFlagSet(args[0], flag.ContinueOnError)
	f.SetOutput(stderr)
	provider := f.String("provider", "", "provider")
	version := f.String("provider-version", "", "observed provider version")
	file := f.String("file", "", "explicit source file")
	namespace := f.String("namespace", "", "shared source namespace")
	stream := f.String("stream-id", "", "persistent logical stream ID")
	conversation := f.String("conversation-id", "", "native ID assertion; verified against exposed source IDs")
	checkpoint := f.String("checkpoint", "", "caller-owned checkpoint JSON")
	root := f.String("root", "", "explicit source root")
	limit := f.Int("limit", 256, "record/file bound")
	raw := f.Bool("raw", false, "include original bytes as base64")
	if err := f.Parse(args[1:]); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected_arguments")
	}
	allowed := map[string]map[string]bool{
		"probe":    {"provider": true, "provider-version": true},
		"discover": {"root": true, "limit": true},
		"validate": {"file": true},
		"inspect":  {"provider": true, "provider-version": true, "namespace": true, "stream-id": true, "conversation-id": true, "file": true, "checkpoint": true, "limit": true, "raw": true},
	}
	if allowed[args[0]] == nil {
		return errors.New("unknown_command; use_skald_help")
	}
	invalidFlag := false
	f.Visit(func(v *flag.Flag) {
		if !allowed[args[0]][v.Name] {
			invalidFlag = true
		}
	})
	if invalidFlag {
		return errors.New("flag_not_supported_for_command")
	}
	switch args[0] {
	case "probe":
		if *provider == "" {
			return errors.New("provider_required")
		}
		return writeJSON(out, sessioncapture.Probe(*provider, *version))
	case "discover":
		if *root == "" {
			return errors.New("root_required")
		}
		paths, err := sessioncapture.Discover(*root, *limit)
		if err != nil {
			return errors.New("source_discovery_failed: " + safeError(err))
		}
		return writeJSON(out, map[string]any{"paths": paths, "live_collection": false})
	case "validate":
		data, err := readBounded(*file, 64<<20)
		if err != nil {
			return err
		}
		if _, err := sessionrecord.Decode(data); err != nil {
			return err
		}
		return writeJSON(out, map[string]any{"valid": true, "contract_version": 1})
	case "inspect":
		var cp sessioncapture.Checkpoint
		if *checkpoint != "" {
			data, err := readBounded(*checkpoint, 64<<10)
			if err != nil {
				return err
			}
			if err := json.Unmarshal(data, &cp); err != nil {
				return errors.New("invalid_checkpoint_json")
			}
		}
		input, err := os.Open(*file)
		if err != nil {
			return errors.New("source_unavailable")
		}
		defer input.Close()
		info, err := input.Stat()
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("regular_source_file_required")
		}
		limits := sessioncapture.DefaultLimits()
		limits.Records = *limit
		batch, err := sessioncapture.Read(input, sessioncapture.Source{Namespace: *namespace, Provider: *provider, StreamID: *stream, ConversationID: *conversation, ProviderVersion: *version}, cp, limits, time.Now())
		if err != nil {
			return errors.New("capture_failed: " + safeError(err))
		}
		// Omit originals from the human inspection default; archive consumers use
		// the package directly or explicitly request the complete raw batch.
		if !*raw {
			for i := range batch.Records {
				batch.Records[i].Raw = nil
			}
		}
		if err := writeJSON(out, batch); err != nil {
			return err
		}
		if batch.Blocked {
			return errors.New("capture_blocked; inspect_gap_codes")
		}
		return nil
	default:
		return errors.New("unknown_command; use_skald_help")
	}
}

// Preserve JSON round-trip values while escaping invisible terminal controls and
// bidi formatting from untrusted source content. Archive bytes remain unchanged.
func writeJSON(out io.Writer, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	var safe strings.Builder
	safe.Grow(len(b) + 1)
	for _, r := range string(b) {
		if unicode.Is(unicode.Cf, r) || (r >= 0x7f && unicode.IsControl(r)) {
			if r <= 0xffff {
				_, err = fmt.Fprintf(&safe, "\\u%04x", r)
			} else {
				a, z := utf16.EncodeRune(r)
				_, err = fmt.Fprintf(&safe, "\\u%04x\\u%04x", a, z)
			}
		} else {
			_, err = safe.WriteRune(r)
		}
		if err != nil {
			return err
		}
	}
	safe.WriteByte('\n')
	_, err = io.WriteString(out, safe.String())
	return err
}

func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("file_unavailable")
	}
	defer f.Close()
	i, err := f.Stat()
	if err != nil || !i.Mode().IsRegular() {
		return nil, errors.New("regular_file_required")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, errors.New("file_read_failed")
	}
	if int64(len(b)) > limit {
		return nil, errors.New("file_too_large")
	}
	return b, nil
}

func safeError(err error) string {
	var p *os.PathError
	if errors.As(err, &p) {
		return "source_io_error"
	}
	return err.Error()
}
