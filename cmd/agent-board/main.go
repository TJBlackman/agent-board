// Command agent-board is the board service, its operator commands, and the
// agent facing HTTP helper, in one static binary.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"trevorblackman.dev/agent-board/internal/api"
	"trevorblackman.dev/agent-board/internal/client"
	"trevorblackman.dev/agent-board/internal/domain"
	"trevorblackman.dev/agent-board/internal/storage"
)

// version is set with -ldflags "-X main.version=...".
var version = "dev"

const usage = `agent-board <command> [flags]

Service and operator commands:
  serve                 Run the HTTP service (env: AGENT_BOARD_DATA_DIR, AGENT_BOARD_LISTEN, AGENT_BOARD_ADMIN_LISTEN,
                        AGENT_BOARD_BACKUP_TIME HH:MM, AGENT_BOARD_TZ, AGENT_BOARD_LOG_LEVEL)
  token create --label L --permission read|write     Issue a credential (prints the token once)
  token list | token revoke <id|label>
  backup now | backup list | verify-backup <dir>
  metrics | rebuild-search | cleanup | rotate-generation      (admin listener, env AGENT_BOARD_ADMIN_URL)
  export --out DIR      Portable logical export (service must be stopped)
  import --from DIR     Import into an empty data directory (service must be stopped)
  restore-prep          After copying a backup into place: revoke every token and rotate the generation
  healthcheck           Exit 0 when the local service answers /healthz
  version

` + client.Usage

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	client.Version = version
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "token":
		err = tokenCmd(args)
	case "backup":
		err = backupCmd(args)
	case "verify-backup":
		err = verifyBackupCmd(args)
	case "metrics":
		err = adminCall("GET", "/admin/metrics", nil)
	case "rebuild-search":
		err = adminCall("POST", "/admin/search/rebuild", nil)
	case "cleanup":
		err = adminCall("POST", "/admin/cleanup", nil)
	case "rotate-generation":
		err = adminCall("POST", "/admin/generation/rotate", nil)
	case "export":
		err = exportCmd(args)
	case "import":
		err = importCmd(args)
	case "restore-prep":
		err = restorePrep()
	case "healthcheck":
		err = healthcheck()
	case "version":
		fmt.Println(version)
	case "help", "-h", "--help":
		fmt.Println(usage)
	default:
		os.Exit(client.Run(os.Args[1:], os.Stdout, os.Stderr))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func dataDir() string { return env("AGENT_BOARD_DATA_DIR", "data") }
func dbPath() string  { return filepath.Join(dataDir(), "board.db") }

func logger() *slog.Logger {
	lvl := slog.LevelInfo
	switch strings.ToLower(env("AGENT_BOARD_LOG_LEVEL", "info")) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}

func serve() error {
	log := logger()
	listen := env("AGENT_BOARD_LISTEN", "127.0.0.1:8080")
	allowHTTP := os.Getenv("AGENT_BOARD_WEB_ALLOW_HTTP")
	if allowHTTP != "" && allowHTTP != "true" && allowHTTP != "false" {
		return errors.New("AGENT_BOARD_WEB_ALLOW_HTTP must be true or false")
	}
	web := api.WebConfig{PrincipalID: os.Getenv("AGENT_BOARD_WEB_PRINCIPAL_ID"), Origin: os.Getenv("AGENT_BOARD_WEB_ORIGIN"), AllowHTTP: allowHTTP == "true"}
	if err := web.Validate(listen); err != nil {
		return err
	}
	ctx := context.Background()
	db, err := storage.Open(ctx, dbPath(), storage.Options{Create: true})
	if err != nil {
		return err
	}
	defer db.Close()
	loc := time.UTC
	if tz := os.Getenv("AGENT_BOARD_TZ"); tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		} else {
			log.Warn("invalid AGENT_BOARD_TZ, using UTC", slog.String("value", tz))
		}
	}
	srv, err := api.New(api.Config{
		Listen:      listen,
		Web:         web,
		AdminListen: env("AGENT_BOARD_ADMIN_LISTEN", "127.0.0.1:8081"),
		DataDir:     dataDir(),
		BackupDir:   env("AGENT_BOARD_BACKUP_DIR", filepath.Join(filepath.Dir(dataDir()), "backups")),
		BackupTime:  os.Getenv("AGENT_BOARD_BACKUP_TIME"),
		Location:    loc,
		Version:     version,
		Log:         log,
	}, db)
	if err != nil {
		return err
	}
	if err := srv.Start(); err != nil {
		return err
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	s := <-sig
	log.Info("shutting down", slog.String("signal", s.String()))
	sctx, cancel := context.WithTimeout(ctx, domain.ShutdownDrain)
	defer cancel()
	return srv.Shutdown(sctx)
}

func adminURL() string {
	return strings.TrimRight(env("AGENT_BOARD_ADMIN_URL", "http://127.0.0.1:8081"), "/")
}

func adminRequest(method, path string, body any) ([]byte, int, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, adminURL()+path, rd)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c := &http.Client{Timeout: 15 * time.Minute}
	resp, err := c.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("admin listener unreachable at %s (run inside the service container): %w", adminURL(), err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return b, resp.StatusCode, nil
}

func adminCall(method, path string, body any) error {
	b, status, err := adminRequest(method, path, body)
	if err != nil {
		return err
	}
	var v any
	if json.Unmarshal(b, &v) == nil {
		pretty, _ := json.MarshalIndent(v, "", "  ")
		fmt.Println(string(pretty))
	} else {
		fmt.Println(string(b))
	}
	if status >= 400 {
		return fmt.Errorf("HTTP %d", status)
	}
	return nil
}

func tokenCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("token create|list|revoke")
	}
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("token create", flag.ContinueOnError)
		label := fs.String("label", "", "principal label")
		perm := fs.String("permission", "write", "read or write")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *label == "" {
			return errors.New("--label is required")
		}
		b, status, err := adminRequest("POST", "/admin/tokens", map[string]string{"label": *label, "permission": *perm})
		if err != nil {
			return err
		}
		if status != http.StatusCreated {
			return fmt.Errorf("HTTP %d: %s", status, string(b))
		}
		var res struct {
			Token       string `json:"token"`
			PrincipalID string `json:"principal_id"`
		}
		if err := json.Unmarshal(b, &res); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "principal %s (%s, %s) created; the token follows on stdout exactly once\n", res.PrincipalID, *label, *perm)
		fmt.Println(res.Token)
		return nil
	case "list":
		return adminCall("GET", "/admin/tokens", nil)
	case "revoke":
		if len(args) != 2 {
			return errors.New("token revoke <id|label>")
		}
		return adminCall("DELETE", "/admin/tokens/"+args[1], nil)
	}
	return errors.New("token create|list|revoke")
}

func backupCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("backup now|list")
	}
	switch args[0] {
	case "now":
		return adminCall("POST", "/admin/backups", nil)
	case "list":
		return adminCall("GET", "/admin/backups", nil)
	}
	return errors.New("backup now|list")
}

func verifyBackupCmd(args []string) error {
	if len(args) != 1 {
		return errors.New("verify-backup <dir>")
	}
	m, err := storage.VerifyBackup(args[0])
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	fmt.Println(string(b))
	return nil
}

func exportCmd(args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	out := fs.String("out", "", "destination directory (must be empty or absent)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("--out is required")
	}
	db, err := storage.Open(context.Background(), dbPath(), storage.Options{Exclusive: true})
	if err != nil {
		return err
	}
	defer db.Close()
	m, err := db.Export(context.Background(), *out)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	fmt.Println(string(b))
	return nil
}

func importCmd(args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	from := fs.String("from", "", "export directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *from == "" {
		return errors.New("--from is required")
	}
	if info, err := os.Stat(dbPath()); err == nil && info.Size() > 0 {
		return fmt.Errorf("%s already exists; import only into an empty data directory", dbPath())
	}
	db, err := storage.Open(context.Background(), dbPath(), storage.Options{Exclusive: true, Create: true})
	if err != nil {
		return err
	}
	defer db.Close()
	m, err := db.Import(context.Background(), *from)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	fmt.Println(string(b))
	fmt.Fprintln(os.Stderr, "import complete: new board instance; all principals are revoked, issue fresh tokens with `agent-board token create` after starting the service")
	return nil
}

func restorePrep() error {
	db, err := storage.Open(context.Background(), dbPath(), storage.Options{Exclusive: true})
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()
	n, err := db.RevokeAllPrincipals(ctx)
	if err != nil {
		return err
	}
	gen, err := db.RotateGeneration(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("revoked %d credentials; new generation %s; cursors issued before the restore are now invalid\n", n, gen)
	return nil
}

func healthcheck() error {
	addr := env("AGENT_BOARD_LISTEN", "127.0.0.1:8080")
	if strings.HasPrefix(addr, ":") || strings.HasPrefix(addr, "0.0.0.0:") {
		addr = "127.0.0.1" + addr[strings.LastIndex(addr, ":"):]
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("healthz returned %d", resp.StatusCode)
	}
	return nil
}
