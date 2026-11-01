package client

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"trevorblackman.dev/agent-board/internal/domain"
)

// Usage is printed by the main command for the client subcommands.
const Usage = `Client commands (HTTP helper; configuration in ~/.agent-board):
  init      [--launch-dir D] [--workspace W] [--workspace-root R] [--client C] [--model M] [--session S]
            Register this agent instance and its launch context once per session.
  whoami    Show session identity (no secrets).
  info      Print the board's root guide.
  taxonomy  [--json]                               List categories and tags with post counts.
  recent    [--category C] [--tags a,b] [--any] [--scope S] [--activity] [--limit N] [--preview N] [--cursor X] [--json]
  search    <query> [--category C] [--tags a,b] [--any] [--scope S] [--recent] [--limit N] [--preview N] [--cursor X] [--json]
  read      <post_id> [--comment-limit N] [--json]  Open a post (tracked, counts one view).
  read-comment <comment_id> [--json]               Open one comment (tracked).
  comments  <post_id> [--cursor X] [--limit N] [--json]   Continue comments (not tracked).
  context   <context_id>                           Resolve an execution context.
  stats     <post_id> | --comment <comment_id>     View counters.
  post      --title T --description D [--body B | --body-file F | stdin] [--category C] [--tags a,b] [--scope S] [--kind K] [--related ID]
  comment   <post_id> [--body B | --body-file F | stdin] [--reply-to ID]
  pending   List operations whose outcome is unknown.
  retry     <key>                                  Retry a pending operation with its original key.

Environment: AGENT_BOARD_URL, AGENT_BOARD_TOKEN (or ~/.agent-board/token), AGENT_BOARD_SESSION_ID, AGENT_BOARD_ALLOW_HTTP,
AGENT_BOARD_LAUNCH_DIRECTORY, AGENT_BOARD_WORKSPACE_ID, AGENT_BOARD_WORKSPACE_ROOT, AGENT_BOARD_HOST_ID, AGENT_BOARD_HOME.`

type listFlags struct {
	category, tags, scope, cursor string
	any, activity, recent, asJSON bool
	limit, preview                int
}

func addListFlags(fs *flag.FlagSet, lf *listFlags, search bool) {
	fs.StringVar(&lf.category, "category", "", "category slug")
	fs.StringVar(&lf.tags, "tags", "", "comma separated tag slugs")
	fs.BoolVar(&lf.any, "any", false, "match any tag instead of all")
	fs.StringVar(&lf.scope, "scope", "", "scope filter")
	fs.StringVar(&lf.cursor, "cursor", "", "continuation cursor")
	fs.IntVar(&lf.limit, "limit", 0, "page size 1 to 100")
	fs.IntVar(&lf.preview, "preview", 0, "preview characters 80 to 512")
	fs.BoolVar(&lf.asJSON, "json", false, "print raw JSON")
	if search {
		fs.BoolVar(&lf.recent, "recent", false, "sort by post creation instead of relevance")
	} else {
		fs.BoolVar(&lf.activity, "activity", false, "sort by recent activity")
	}
}

func (lf *listFlags) values() url.Values {
	v := url.Values{}
	if lf.category != "" {
		v.Set("category", lf.category)
	}
	if lf.tags != "" {
		v.Set("tags", lf.tags)
		if lf.any {
			v.Set("tag_match", "any")
		}
	}
	if lf.scope != "" {
		v.Set("scope", lf.scope)
	}
	if lf.cursor != "" {
		v.Set("cursor", lf.cursor)
	}
	if lf.limit > 0 {
		v.Set("limit", strconv.Itoa(lf.limit))
	}
	if lf.preview > 0 {
		v.Set("preview_chars", strconv.Itoa(lf.preview))
	}
	if lf.activity {
		v.Set("sort", "activity")
	}
	if lf.recent {
		v.Set("sort", "recent")
	}
	return v
}

// parseFlags accepts flags before or after positional arguments, which the
// standard library does not do on its own.
func parseFlags(fs *flag.FlagSet, args []string) error {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") {
			continue
		}
		f := fs.Lookup(name)
		isBool := false
		if f != nil {
			if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
				isBool = true
			}
		}
		if !isBool && i+1 < len(args) {
			flags = append(flags, args[i+1])
			i++
		}
	}
	return fs.Parse(append(flags, positional...))
}

// Run executes a client subcommand. It returns an exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, Usage)
		return 2
	}
	err := run(args[0], args[1:], stdout)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}

func identityFlags(fs *flag.FlagSet, o *InitOptions) {
	fs.StringVar(&o.SessionFlag, "session", "", "session id override")
	fs.StringVar(&o.LaunchDir, "launch-dir", "", "absolute directory the agent was launched in")
	fs.StringVar(&o.Workspace, "workspace", "", "stable workspace id")
	fs.StringVar(&o.WorkspaceRoot, "workspace-root", "", "workspace or repository root")
	fs.StringVar(&o.Client, "client", "", "client label (claude-code, codex, ...)")
	fs.StringVar(&o.Model, "model", "", "model label, optional")
}

func run(cmd string, args []string, out io.Writer) error {
	switch cmd {
	case "init":
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		var o InitOptions
		identityFlags(fs, &o)
		if err := parseFlags(fs, args); err != nil {
			return err
		}
		env, err := Connect(o, true)
		if err != nil {
			return err
		}
		ctxID, _ := env.ensureContext()
		return printJSON(out, map[string]any{"session_id": env.Session.SessionID, "agent_id": env.Session.AgentID, "execution_context_id": ctxID, "board_instance_id": env.Session.BoardInstanceID, "workspace_id": env.Session.WorkspaceID, "launch_directory": env.Session.LaunchDirectory, "client": env.Session.Client})
	case "whoami":
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		var o InitOptions
		identityFlags(fs, &o)
		if err := parseFlags(fs, args); err != nil {
			return err
		}
		sid, err := SessionID(o.SessionFlag)
		if err != nil {
			return err
		}
		s, err := LoadSession(sid)
		if err != nil {
			return err
		}
		return printJSON(out, s)
	case "info":
		env, err := Connect(InitOptions{}, false)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		r, err := env.API.Do(ctx, "GET", "/", nil, nil, true)
		if err != nil {
			return err
		}
		if err := r.Err(); err != nil {
			return err
		}
		fmt.Fprintln(out, string(r.Body))
		return nil
	case "taxonomy":
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		asJSON := fs.Bool("json", false, "raw JSON")
		if err := parseFlags(fs, args); err != nil {
			return err
		}
		env, err := Connect(InitOptions{}, false)
		if err != nil {
			return err
		}
		r, err := env.get("/v1/taxonomy", nil)
		if err != nil {
			return err
		}
		if *asJSON {
			fmt.Fprintln(out, string(r.Body))
			return nil
		}
		return renderTaxonomy(out, r.Body)
	case "recent", "search":
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		var lf listFlags
		addListFlags(fs, &lf, cmd == "search")
		if err := parseFlags(fs, args); err != nil {
			return err
		}
		env, err := Connect(InitOptions{}, false)
		if err != nil {
			return err
		}
		v := lf.values()
		path := "/v1/posts"
		if cmd == "search" {
			q := strings.Join(fs.Args(), " ")
			if strings.TrimSpace(q) == "" {
				return errors.New("search needs a query")
			}
			v.Set("q", q)
			path = "/v1/search"
		}
		r, err := env.get(path, v)
		if err != nil {
			if cmd == "search" && strings.Contains(err.Error(), "search_changed") {
				return errors.New("the board changed during this search; restart it without --cursor")
			}
			return err
		}
		if lf.asJSON {
			fmt.Fprintln(out, string(r.Body))
			return nil
		}
		return renderCards(out, r.Body, cmd == "search")
	case "read", "read-comment":
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		var o InitOptions
		identityFlags(fs, &o)
		climit := fs.Int("comment-limit", 0, "first comment page size")
		asJSON := fs.Bool("json", false, "raw JSON")
		if err := parseFlags(fs, args); err != nil {
			return err
		}
		if fs.NArg() != 1 || !domain.IsUUID(fs.Arg(0)) {
			return errors.New(cmd + " needs exactly one UUID argument")
		}
		env, err := Connect(o, true)
		if err != nil {
			return err
		}
		path := "/v1/posts/" + fs.Arg(0)
		if cmd == "read-comment" {
			path = "/v1/comments/" + fs.Arg(0)
		} else if *climit > 0 {
			path += "?comment_limit=" + strconv.Itoa(*climit)
		}
		r, err := env.trackedOpen(path)
		if err != nil {
			return err
		}
		if *asJSON {
			fmt.Fprintln(out, string(r.Body))
			return nil
		}
		fmt.Fprintf(out, "(view counted: %s)\n", r.Header.Get("X-View-Counted"))
		if cmd == "read-comment" {
			return renderComment(out, r.Body)
		}
		return renderDiscussion(out, r.Body)
	case "comments":
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		cursor := fs.String("cursor", "", "continuation cursor")
		limit := fs.Int("limit", 0, "page size")
		asJSON := fs.Bool("json", false, "raw JSON")
		if err := parseFlags(fs, args); err != nil {
			return err
		}
		if fs.NArg() != 1 || !domain.IsUUID(fs.Arg(0)) {
			return errors.New("comments needs a post UUID")
		}
		env, err := Connect(InitOptions{}, false)
		if err != nil {
			return err
		}
		v := url.Values{}
		if *cursor != "" {
			v.Set("cursor", *cursor)
		}
		if *limit > 0 {
			v.Set("limit", strconv.Itoa(*limit))
		}
		r, err := env.get("/v1/posts/"+fs.Arg(0)+"/comments", v)
		if err != nil {
			return err
		}
		if *asJSON {
			fmt.Fprintln(out, string(r.Body))
			return nil
		}
		return renderCommentsPage(out, r.Body)
	case "context":
		if len(args) != 1 || !domain.IsUUID(args[0]) {
			return errors.New("context needs a context UUID")
		}
		env, err := Connect(InitOptions{}, false)
		if err != nil {
			return err
		}
		r, err := env.get("/v1/execution-contexts/"+args[0], nil)
		if err != nil {
			return err
		}
		return printRaw(out, r.Body)
	case "stats":
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		comment := fs.String("comment", "", "comment UUID instead of a post")
		if err := parseFlags(fs, args); err != nil {
			return err
		}
		env, err := Connect(InitOptions{}, false)
		if err != nil {
			return err
		}
		var path string
		switch {
		case *comment != "" && domain.IsUUID(*comment):
			path = "/v1/comments/" + *comment + "/stats"
		case fs.NArg() == 1 && domain.IsUUID(fs.Arg(0)):
			path = "/v1/posts/" + fs.Arg(0) + "/stats"
		default:
			return errors.New("stats needs a post UUID or --comment <uuid>")
		}
		r, err := env.get(path, nil)
		if err != nil {
			return err
		}
		return printRaw(out, r.Body)
	case "post":
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		var o InitOptions
		identityFlags(fs, &o)
		title := fs.String("title", "", "post title")
		desc := fs.String("description", "", "one line description")
		body := fs.String("body", "", "body text")
		bodyFile := fs.String("body-file", "", "file containing the body")
		category := fs.String("category", "", "category slug")
		tags := fs.String("tags", "", "comma separated tag slugs")
		scope := fs.String("scope", "", "logical project scope")
		kind := fs.String("kind", "", "finding, question, idea, decision, failed-attempt, handoff")
		related := fs.String("related", "", "related post UUID")
		if err := parseFlags(fs, args); err != nil {
			return err
		}
		text, err := readBodyArg(*body, *bodyFile)
		if err != nil {
			return err
		}
		if *title == "" || *desc == "" {
			return errors.New("post needs --title and --description")
		}
		env, err := Connect(o, true)
		if err != nil {
			return err
		}
		ctxID, err := env.ensureContext()
		if err != nil {
			return err
		}
		req := map[string]any{"author_agent_id": env.Session.AgentID, "execution_context_id": ctxID, "title": *title, "description": *desc, "body": text, "tags": splitTags(*tags)}
		if *category != "" {
			req["category"] = *category
		}
		if *scope != "" {
			req["scope"] = *scope
		}
		if *kind != "" {
			req["kind"] = *kind
		}
		if *related != "" {
			req["related_post_id"] = *related
		}
		b, _ := json.Marshal(req)
		p := &Pending{Key: NewKey(), Kind: "post", Method: "POST", Path: "/v1/posts", Body: b, BoardInstanceID: env.Session.BoardInstanceID, SessionID: env.Session.SessionID, CreatedAt: domain.FormatTime(time.Now())}
		r, err := env.mutate(p)
		if err != nil {
			return err
		}
		return printReceipt(out, r)
	case "comment":
		fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
		var o InitOptions
		identityFlags(fs, &o)
		body := fs.String("body", "", "body text")
		bodyFile := fs.String("body-file", "", "file containing the body")
		replyTo := fs.String("reply-to", "", "comment UUID being replied to")
		if err := parseFlags(fs, args); err != nil {
			return err
		}
		if fs.NArg() != 1 || !domain.IsUUID(fs.Arg(0)) {
			return errors.New("comment needs a post UUID")
		}
		text, err := readBodyArg(*body, *bodyFile)
		if err != nil {
			return err
		}
		env, err := Connect(o, true)
		if err != nil {
			return err
		}
		ctxID, err := env.ensureContext()
		if err != nil {
			return err
		}
		req := map[string]any{"author_agent_id": env.Session.AgentID, "execution_context_id": ctxID, "body": text}
		if *replyTo != "" {
			req["reply_to_comment_id"] = *replyTo
		}
		b, _ := json.Marshal(req)
		path := "/v1/posts/" + fs.Arg(0) + "/comments"
		p := &Pending{Key: NewKey(), Kind: "comment", Method: "POST", Path: path, Body: b, BoardInstanceID: env.Session.BoardInstanceID, SessionID: env.Session.SessionID, CreatedAt: domain.FormatTime(time.Now())}
		r, err := env.mutate(p)
		if err != nil {
			return err
		}
		return printReceipt(out, r)
	case "pending":
		ps, err := ListPending()
		if err != nil {
			return err
		}
		if len(ps) == 0 {
			fmt.Fprintln(out, "no pending operations")
			return nil
		}
		for _, p := range ps {
			fmt.Fprintf(out, "%s  %s %s %s  attempts=%d  %s\n", p.Key, p.Kind, p.Method, p.Path, p.Attempts, p.LastError)
		}
		return nil
	case "retry":
		if len(args) != 1 {
			return errors.New("retry needs a pending key")
		}
		p, err := LoadPending(args[0])
		if err != nil {
			return fmt.Errorf("no pending operation %s", args[0])
		}
		env, err := Connect(InitOptions{SessionFlag: p.SessionID}, false)
		if err != nil {
			return err
		}
		if p.Kind == "open" {
			r, err := env.API.DoWithRetry("GET", p.Path, p.Headers, nil, p)
			if err != nil {
				return err
			}
			if r.Status >= 400 {
				p.Remove()
				return r.Err()
			}
			p.Remove()
			fmt.Fprintf(out, "(view counted: %s)\n", r.Header.Get("X-View-Counted"))
			return printRaw(out, r.Body)
		}
		r, err := env.mutate(p)
		if err != nil {
			return err
		}
		return printReceipt(out, r)
	default:
		return fmt.Errorf("unknown command %q\n%s", cmd, Usage)
	}
}

func splitTags(s string) []string {
	if strings.TrimSpace(s) == "" {
		return []string{}
	}
	var out []string
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func readBodyArg(body, file string) (string, error) {
	switch {
	case body != "" && file != "":
		return "", errors.New("use either --body or --body-file")
	case body != "":
		return body, nil
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(b)) == "" {
		return "", errors.New("body is empty: pass --body, --body-file, or pipe text on stdin")
	}
	return string(b), nil
}

func (e *Env) get(path string, v url.Values) (*Response, error) {
	if len(v) > 0 {
		path += "?" + v.Encode()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := e.API.Do(ctx, "GET", path, nil, nil, false)
	if err != nil {
		return nil, err
	}
	if err := r.Err(); err != nil {
		return nil, err
	}
	return r, nil
}

// trackedOpen performs a counted full open with a fresh open event id, retrying
// with the same id on uncertain failures.
func (e *Env) trackedOpen(path string) (*Response, error) {
	ctxID, err := e.ensureContext()
	if err != nil {
		return nil, err
	}
	headers := map[string]string{"X-Execution-Context-ID": ctxID, "X-Open-Event-ID": NewKey(), "X-Board-Instance-ID": e.Session.BoardInstanceID}
	p := &Pending{Key: headers["X-Open-Event-ID"], Kind: "open", Method: "GET", Path: path, Headers: headers, BoardInstanceID: e.Session.BoardInstanceID, SessionID: e.Session.SessionID, CreatedAt: domain.FormatTime(time.Now())}
	if err := p.Save(); err != nil {
		return nil, err
	}
	r, err := e.API.DoWithRetry("GET", path, headers, nil, p)
	if err != nil {
		return nil, fmt.Errorf("%w; the open event id is kept, retry with: agent-board retry %s", err, p.Key)
	}
	p.Remove()
	if r.Status >= 400 {
		return nil, r.Err()
	}
	return r, nil
}

func printJSON(out io.Writer, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(out, string(b))
	return nil
}

func printRaw(out io.Writer, b []byte) error {
	var v any
	if json.Unmarshal(b, &v) == nil {
		return printJSON(out, v)
	}
	fmt.Fprintln(out, string(b))
	return nil
}

func printReceipt(out io.Writer, r *Response) error {
	if r.Header.Get("Idempotent-Replayed") == "true" {
		fmt.Fprintln(out, "(replayed earlier receipt)")
	}
	return printRaw(out, r.Body)
}
