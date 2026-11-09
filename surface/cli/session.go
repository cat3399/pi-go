package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/cat3399/pi-go/internal/agent"
	"github.com/cat3399/pi-go/internal/app"
	"github.com/cat3399/pi-go/internal/application"
	"github.com/cat3399/pi-go/internal/session"
)

const sessionHelp = `Usage: pi-go session [--id ID] [--agent-dir DIR] [--file JSONL] [--json] <command>

Commands:
  list             List saved sessions and the current live session
  history --help   Read original dialogue, raw entries, or search history
  context --help   Inspect usage, submit a short handoff, or request one

An explicit --id selects a session; otherwise PI_SESSION_ID selects the current
session. Agent bash calls connect to the running owner automatically. Outside
an agent, saved sessions are resolved through --agent-dir. --file reads an
explicit JSONL without opening a writer. Accepted changes run after the current
tool batch; context status reports completion. Output is never truncated here.
`

const historyHelp = `Usage: pi-go session [options] history <read|search|windows> [options]

  read [--view dialogue|raw] [--entry ID ...]
  search QUERY [--view dialogue|raw] [--case-sensitive]
  windows

Range options: --window initial|current|ID, --leaf ID, --from ID, --to ID,
--limit N (complete matching records; 0 means all). Entry ranges are inclusive.
The default scope is the current branch across all windows. Dialogue keeps user
and assistant visible text, original phase labels and attachment references;
it excludes thinking, tool calls, tool results and synthetic messages. Raw
reads preserve the complete original JSON. Search returns matching original
lines with entry ID, block index and line number; use read --entry ID to expand.
Long output uses bash's existing full-output file and read continuation.
`

const contextHelp = `Usage: pi-go session [options] context <status|new|compact> [options]

  status
  new --handoff-stdin | --handoff-file PATH | --handoff TEXT
      [--request-id ID]
  compact [--instructions TEXT]

new uses an already-written short handoff, without another model call or any
automatically retained old conversation. The original history stays readable.
Submit progress, relevant files/modules and next direction; no fixed template
is required. A stable request ID makes handoff retries idempotent on this branch.
compact asks the current model for a brief handoff using the active context,
then starts a new window with original history available on demand. During a run,
both commands return accepted before changing context at a settled turn boundary.
status reports estimated tokens (not exact provider tokenization) and the latest
change state. --file is only for history reads; live control uses PI_SESSION_*.
`

func RunSession(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || sessionIsHelp(args[0]) {
		fmt.Fprint(stdout, sessionHelp)
		return 0
	}
	root := flag.NewFlagSet("pi-go session", flag.ContinueOnError)
	root.SetOutput(stderr)
	id := root.String("id", os.Getenv("PI_SESSION_ID"), "session ID")
	agentDir := root.String("agent-dir", os.Getenv("PI_SESSION_AGENT_DIR"), "agent data directory")
	file := root.String("file", "", "read an explicit session JSONL")
	jsonOutput := root.Bool("json", false, "machine-readable output")
	if err := root.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	args = root.Args()
	if len(args) == 0 {
		fmt.Fprint(stdout, sessionHelp)
		return 0
	}
	fail := func(err error) int { fmt.Fprintln(stderr, "pi-go session:", err); return 1 }
	endpoint, token := os.Getenv("PI_SESSION_ENDPOINT"), os.Getenv("PI_SESSION_TOKEN")
	live := endpoint != "" && *id == os.Getenv("PI_SESSION_ID") && *file == ""
	call := func(action string, query session.HistoryViewQuery, change agent.ContextChangeRequest) (sessionControlResponse, error) {
		return callSessionControl(ctx, endpoint, token, sessionControlRequest{SessionID: *id, Action: action, Query: query, Change: change})
	}
	openService := func() (*application.Service, error) {
		return application.NewService(application.ServiceOptions{Context: ctx, Production: app.ProductionConfig{AgentDir: *agentDir}, DisableReaper: true})
	}
	if args[0] == "list" {
		if len(args) != 1 {
			return fail(fmt.Errorf("list takes no arguments"))
		}
		service, err := openService()
		if err != nil {
			return fail(err)
		}
		defer service.Close(context.Background())
		values, err := service.ListSessions()
		if err != nil {
			return fail(err)
		}
		if live {
			response, err := call("info", session.HistoryViewQuery{}, agent.ContextChangeRequest{})
			if err != nil {
				return fail(err)
			}
			found := false
			for i := range values {
				if values[i].ID == response.Info.ID {
					values[i], found = *response.Info, true
				}
			}
			if !found {
				values = append(values, *response.Info)
			}
		}
		if *jsonOutput {
			return writeSessionJSON(stdout, stderr, values)
		}
		for _, value := range values {
			fmt.Fprintf(stdout, "%s\t%s\t%s\n", value.ID, value.CWD, value.Name)
		}
		return 0
	}
	if args[0] != "history" && args[0] != "context" {
		return fail(fmt.Errorf("unknown command %q", args[0]))
	}
	if len(args) < 2 || sessionIsHelp(args[1]) {
		if args[0] == "history" {
			fmt.Fprint(stdout, historyHelp)
		} else {
			fmt.Fprint(stdout, contextHelp)
		}
		return 0
	}
	if args[0] == "history" {
		query := session.HistoryViewQuery{View: "dialogue", IgnoreCase: true}
		flags := flag.NewFlagSet("history "+args[1], flag.ContinueOnError)
		flags.SetOutput(stderr)
		flags.StringVar(&query.View, "view", "dialogue", "dialogue or raw")
		flags.StringVar(&query.WindowID, "window", "", "context window ID")
		flags.StringVar(&query.FromID, "from", "", "inclusive first entry")
		flags.StringVar(&query.ToID, "to", "", "inclusive last entry")
		flags.IntVar(&query.Limit, "limit", 0, "maximum complete records (0 means all)")
		flags.Func("entry", "entry ID (repeatable)", func(value string) error { query.EntryIDs = append(query.EntryIDs, value); return nil })
		flags.Func("leaf", "branch leaf ID", func(value string) error { query.LeafID = &value; return nil })
		caseSensitive := flags.Bool("case-sensitive", false, "case-sensitive search")
		if err := parseSessionLeafFlags(flags, args[2:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			return 2
		}
		query.IgnoreCase = !*caseSensitive
		if *id == "" && *file == "" {
			return fail(fmt.Errorf("select --id or run from an agent bash call"))
		}
		switch args[1] {
		case "read":
			if flags.NArg() != 0 {
				return fail(fmt.Errorf("read takes no positional arguments"))
			}
		case "windows":
			if flags.NArg() != 0 {
				return fail(fmt.Errorf("windows takes no positional arguments"))
			}
			query.View = "windows"
		case "search":
			if flags.NArg() != 1 || flags.Arg(0) == "" {
				return fail(fmt.Errorf("search requires one non-empty query"))
			}
			query.Search = flags.Arg(0)
		default:
			return fail(fmt.Errorf("unknown history command %q", args[1]))
		}
		var value session.HistoryView
		var err error
		if live {
			var response sessionControlResponse
			response, err = call("history", query, agent.ContextChangeRequest{})
			if err == nil {
				value = *response.History
			}
		} else if *file != "" {
			value, err = application.ReadHistoryFile(*file, query)
			if err == nil && rootIDExplicit(root) && value.SessionID != *id {
				err = fmt.Errorf("file session ID does not match --id")
			}
		} else {
			var service *application.Service
			service, err = openService()
			if err == nil {
				defer service.Close(context.Background())
				value, err = service.ReadHistoryView(*id, query)
			}
		}
		if err != nil {
			return fail(err)
		}
		if *jsonOutput {
			return writeSessionJSON(stdout, stderr, value)
		}
		writeHistoryView(stdout, value)
		return 0
	}
	if *file != "" {
		return fail(fmt.Errorf("--file is read-only; select a session with --id for context commands"))
	}
	request := agent.ContextChangeRequest{}
	flags := flag.NewFlagSet("context "+args[1], flag.ContinueOnError)
	flags.SetOutput(stderr)
	var handoff, handoffFile string
	var handoffStdin bool
	if args[1] == "new" {
		request.Mode = "handoff"
		flags.StringVar(&handoff, "handoff", "", "short handoff text")
		flags.StringVar(&handoffFile, "handoff-file", "", "read handoff text from file")
		flags.BoolVar(&handoffStdin, "handoff-stdin", false, "read handoff text from stdin")
		flags.StringVar(&request.ID, "request-id", "", "stable handoff retry ID")
	} else if args[1] == "compact" {
		request.Mode = "compact"
		flags.StringVar(&request.Instructions, "instructions", "", "summary instructions")
	} else if args[1] != "status" {
		return fail(fmt.Errorf("unknown context command %q", args[1]))
	}
	if err := parseSessionLeafFlags(flags, args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		return fail(fmt.Errorf("unexpected context arguments"))
	}
	if *id == "" {
		return fail(fmt.Errorf("select --id or run from an agent bash call"))
	}
	if request.Mode == "handoff" {
		count := 0
		flags.Visit(func(f *flag.Flag) {
			if f.Name == "handoff" || f.Name == "handoff-file" || f.Name == "handoff-stdin" {
				count++
			}
		})
		if count != 1 {
			return fail(fmt.Errorf("new requires exactly one handoff source"))
		}
		var data []byte
		var err error
		if handoffStdin {
			data, err = io.ReadAll(stdin)
		} else if handoffFile != "" {
			data, err = os.ReadFile(handoffFile)
		} else {
			data = []byte(handoff)
		}
		if err != nil {
			return fail(err)
		}
		request.Handoff = string(data)
	}
	var response sessionControlResponse
	var err error
	if live {
		action := "change"
		if args[1] == "status" {
			action = "status"
		}
		response, err = call(action, session.HistoryViewQuery{}, request)
	} else {
		var service *application.Service
		service, err = openService()
		if err == nil {
			defer service.Close(context.Background())
			var command application.Command = application.ContextChangeCommand{Request: request}
			if args[1] == "status" {
				command = application.ContextStatusCommand{}
			}
			var result application.CommandResult
			result, err = service.Dispatch(ctx, *id, command)
			switch result := result.(type) {
			case application.ContextStatusResult:
				response.Status = &result.Status
			case application.ContextChangeResult:
				response.Change = &result.Result
			}
		}
	}
	if err != nil {
		return fail(err)
	}
	if response.Status != nil {
		if *jsonOutput {
			return writeSessionJSON(stdout, stderr, response.Status)
		}
		value := response.Status
		fmt.Fprintf(stdout, "Session %s · window %s\nEstimated context: %d tokens; window: %d; reserve: %d; request budget: %d\n", value.SessionID, value.WindowID, value.EstimatedTokens, value.ContextWindow, value.ReserveTokens, value.RequestBudget)
		if value.Change != nil {
			writeContextChange(stdout, *value.Change)
		}
	} else if response.Change != nil {
		if *jsonOutput {
			return writeSessionJSON(stdout, stderr, response.Change)
		}
		writeContextChange(stdout, *response.Change)
	}
	return 0
}

func rootIDExplicit(flags *flag.FlagSet) bool {
	result := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "id" {
			result = true
		}
	})
	return result
}
func sessionIsHelp(value string) bool { return value == "help" || value == "--help" || value == "-h" }
func writeSessionJSON(stdout, stderr io.Writer, value any) int {
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
func writeContextChange(writer io.Writer, value agent.ContextChangeResult) {
	fmt.Fprintf(writer, "%s: %s (request %s", value.Mode, value.State, value.RequestID)
	if value.WindowID != "" {
		fmt.Fprintf(writer, ", window %s", value.WindowID)
	}
	fmt.Fprintln(writer, ")")
	if value.Error != "" {
		fmt.Fprintln(writer, value.Error)
	}
}

func writeHistoryView(writer io.Writer, view session.HistoryView) {
	fmt.Fprintf(writer, "Session %s · branch leaf %s · view %s\n", view.SessionID, view.LeafID, view.View)
	if view.View == "windows" {
		for _, window := range view.Windows {
			fmt.Fprintf(writer, "\nWindow %s · %s · previous %s · before entry %s\n", window.ID, window.Method, window.Previous, window.BeforeEntryID)
			if window.Handoff != "" {
				fmt.Fprintln(writer, window.Handoff)
			}
		}
		return
	}
	for _, record := range view.Records {
		fmt.Fprintf(writer, "\n[entry %s | window %s | %s | %s]\n", record.EntryID, record.WindowID, record.Role, record.Timestamp.Format("2006-01-02T15:04:05.000Z07:00"))
		if len(record.Raw) != 0 {
			fmt.Fprintln(writer, string(record.Raw))
		}
		for _, block := range record.Blocks {
			fmt.Fprintf(writer, "[block %d", block.Index)
			if block.Phase != "" {
				fmt.Fprintf(writer, " | phase=%s", block.Phase)
			}
			fmt.Fprintln(writer, "]")
			if block.Type == "text" {
				fmt.Fprintln(writer, block.Text)
			} else {
				fmt.Fprintf(writer, "[%s attachment; reference %s block %d; mime=%s name=%s]\n", block.Type, record.EntryID, block.Index, block.MIMEType, block.Name)
			}
		}
		for _, match := range record.Matches {
			fmt.Fprintf(writer, "[block %d line %d byte %d] %s\n", match.Block, match.Line, match.Offset, match.Text)
		}
	}
	if view.NextEntryID != "" {
		fmt.Fprintf(writer, "\nMore matching records: repeat this query with --from %q\n", view.NextEntryID)
	}
}

// flag.FlagSet normally stops at the first positional argument. Reorder only
// parsed flags so `search "words" --view raw` works without interpreting text
// as shell or stripping literal arguments after --.
func parseSessionLeafFlags(flags *flag.FlagSet, args []string) error {
	var options, positionals []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positionals = append(positionals, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positionals = append(positionals, arg)
			continue
		}
		options = append(options, arg)
		name, _, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		f := flags.Lookup(name)
		if f == nil {
			continue
		}
		boolean, ok := f.Value.(interface{ IsBoolFlag() bool })
		if !hasValue && !(ok && boolean.IsBoolFlag()) && i+1 < len(args) {
			i++
			options = append(options, args[i])
		}
	}
	return flags.Parse(append(append(options, "--"), positionals...))
}
