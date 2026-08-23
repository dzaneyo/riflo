package main

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dzaneyo/riflo/internal/app"
	"github.com/dzaneyo/riflo/internal/config"
	"github.com/dzaneyo/riflo/internal/httpapi"
	"github.com/dzaneyo/riflo/internal/media"
	"github.com/dzaneyo/riflo/internal/store"
	"github.com/dzaneyo/riflo/internal/tasks"
)

const (
	serveShutdownTimeout = 5 * time.Second
	progressInterval     = 500 * time.Millisecond
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stdout)
		return 0
	}
	command := args[0]
	if command == "-h" || command == "--help" || command == "help" {
		if len(args) > 1 {
			printCommandUsage(stdout, args[1])
		} else {
			printUsage(stdout)
		}
		return 0
	}
	switch command {
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "riflo version %s\n", app.Version)
		return 0
	case "doctor":
		return runDoctor(args[1:], stdout, stderr)
	case "serve":
		return runServe(args[1:], stdout, stderr)
	case "inspect":
		return runInspect(args[1:], stdout, stderr)
	case "download":
		return runDownload(args[1:], stdout, stderr)
	case "tasks":
		return runTasks(args[1:], stdout, stderr)
	case "task":
		return runTask(args[1:], stdout, stderr)
	case "cancel":
		return runCancel(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "riflo: unknown command %q\n\n", command)
		printUsage(stderr)
		return 2
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "riflo - local media inspection and download helper")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  riflo serve [--listen 127.0.0.1:8787] [--data-dir PATH] [--db PATH]")
	fmt.Fprintln(w, "  riflo inspect URL [--referer URL] [--origin ORIGIN] [--user-agent UA] [--cookie COOKIE]")
	fmt.Fprintln(w, "  riflo download URL [options]")
	fmt.Fprintln(w, "  riflo tasks [--server URL] [--status STATUS]")
	fmt.Fprintln(w, "  riflo task TASK_ID [--server URL]")
	fmt.Fprintln(w, "  riflo cancel TASK_ID [--server URL]")
	fmt.Fprintln(w, "  riflo doctor")
	fmt.Fprintln(w, "  riflo version")
}

func printCommandUsage(w io.Writer, command string) {
	switch command {
	case "serve":
		fmt.Fprintln(w, "Usage: riflo serve [--listen 127.0.0.1:8787] [--data-dir PATH] [--db PATH] [--downloads PATH]")
	case "inspect":
		fmt.Fprintln(w, "Usage: riflo inspect URL [--referer URL] [--origin ORIGIN] [--user-agent UA] [--cookie COOKIE]")
	case "download":
		fmt.Fprintln(w, "Usage: riflo download URL [--output-dir PATH] [--output-name NAME] [--referer URL] [--origin ORIGIN] [--user-agent UA] [--cookie COOKIE] [--format mp4|mkv] [--hls-variant-index INDEX]")
	case "tasks":
		fmt.Fprintln(w, "Usage: riflo tasks [--server URL] [--status STATUS]")
	case "task":
		fmt.Fprintln(w, "Usage: riflo task TASK_ID [--server URL]")
	case "cancel":
		fmt.Fprintln(w, "Usage: riflo cancel TASK_ID [--server URL]")
	default:
		printUsage(w)
	}
}

func runDoctor(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("doctor", stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "riflo doctor does not accept positional arguments")
		return 2
	}
	cfg, err := config.Defaults()
	if err != nil {
		fmt.Fprintf(stderr, "doctor: %v\n", err)
		return 1
	}
	report := app.RunDoctor(context.Background(), cfg)
	for _, check := range report.Checks {
		label := "ok"
		if !check.OK {
			label = "fail"
		}
		if check.Detail == "" {
			fmt.Fprintf(stdout, "[%s] %s\n", label, check.Name)
		} else {
			fmt.Fprintf(stdout, "[%s] %s: %s\n", label, check.Name, check.Detail)
		}
	}
	if !report.OK {
		return 1
	}
	return 0
}

func runServe(args []string, stdout, stderr io.Writer) int {
	cfg, err := config.Defaults()
	if err != nil {
		fmt.Fprintf(stderr, "serve: %v\n", err)
		return 1
	}
	fs := newFlagSet("serve", stderr)
	listen := fs.String("listen", cfg.ListenAddr, "loopback address to listen on")
	dataDir := fs.String("data-dir", cfg.DataDir, "riflo data directory")
	dbPath := fs.String("db", cfg.DBPath, "SQLite database path")
	downloads := fs.String("downloads", cfg.DownloadsDir, "default downloads directory")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "serve does not accept positional arguments")
		return 2
	}
	cfg.ListenAddr = *listen
	cfg.DataDir = *dataDir
	cfg.DBPath = *dbPath
	cfg.DownloadsDir = *downloads
	if err := cfg.EnsureDirs(); err != nil {
		fmt.Fprintf(stderr, "serve: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	database, err := store.Open(cfg.DBPath)
	if err != nil {
		fmt.Fprintf(stderr, "serve: open database: %v\n", err)
		return 1
	}
	closeStore := func(code int) int {
		if closeErr := database.Close(); closeErr != nil {
			fmt.Fprintf(stderr, "serve: close database: %v\n", closeErr)
			if code == 0 {
				return 1
			}
		}
		return code
	}

	runner := media.DefaultRunner()
	taskService, err := tasks.New(ctx, tasks.Config{
		Store:            database,
		Runner:           runner,
		DefaultOutputDir: cfg.DownloadsDir,
		Concurrency:      2,
	})
	if err != nil {
		fmt.Fprintf(stderr, "serve: initialize tasks: %v\n", err)
		return closeStore(1)
	}
	closeResources := func(code int) int {
		if closeErr := taskService.Close(); closeErr != nil {
			fmt.Fprintf(stderr, "serve: close tasks: %v\n", closeErr)
			if code == 0 {
				code = 1
			}
		}
		return closeStore(code)
	}

	server := httpapi.NewServer(httpapi.ServerConfig{Service: taskService, Version: app.Version})
	httpServer, err := server.HTTPServer(cfg.ListenAddr)
	if err != nil {
		fmt.Fprintf(stderr, "serve: %v\n", err)
		return closeResources(2)
	}
	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		fmt.Fprintf(stderr, "serve: listen: %v\n", err)
		return closeResources(1)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpServer.Serve(listener) }()
	fmt.Fprintf(stdout, "riflo listening on http://%s\n", listener.Addr().String())

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(stderr, "serve: %v\n", err)
			return closeResources(1)
		}
		return closeResources(0)
	case <-ctx.Done():
		// Stop accepting HTTP work before waiting for the task workers and the
		// database. This leaves queued/running task cleanup deterministic.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), serveShutdownTimeout)
		shutdownErr := httpServer.Shutdown(shutdownCtx)
		cancel()
		if shutdownErr != nil {
			fmt.Fprintf(stderr, "serve shutdown: %v\n", shutdownErr)
			_ = httpServer.Close()
		}
		if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(stderr, "serve: %v\n", err)
			return closeResources(1)
		}
		return closeResources(0)
	}
}

func runInspect(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("inspect", stderr)
	referer := fs.String("referer", "", "request Referer")
	origin := fs.String("origin", "", "request Origin")
	userAgent := fs.String("user-agent", "", "request User-Agent")
	cookie := fs.String("cookie", "", "request Cookie (not persisted)")
	urlValue, err := parseURLArgs(fs, args)
	if err != nil {
		return printCommandError(stderr, "inspect", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result, err := media.DefaultRunner().Inspect(ctx, app.InspectRequest{
		URL: urlValue, Referer: *referer, Origin: *origin, UserAgent: *userAgent, Cookie: *cookie,
	})
	if err != nil {
		return printCommandError(stderr, "inspect", err)
	}
	return encodeResult(stdout, result)
}

func runDownload(args []string, stdout, stderr io.Writer) int {
	cfg, err := config.Defaults()
	if err != nil {
		return printCommandError(stderr, "download", err)
	}
	fs := newFlagSet("download", stderr)
	outputDir := fs.String("output-dir", cfg.DownloadsDir, "output directory")
	outputName := fs.String("output-name", "", "output file name")
	referer := fs.String("referer", "", "request Referer")
	origin := fs.String("origin", "", "request Origin")
	userAgent := fs.String("user-agent", "", "request User-Agent")
	cookie := fs.String("cookie", "", "request Cookie (not persisted)")
	format := fs.String("format", "mp4", "output format (mp4 or mkv)")
	hlsVariantIndex := fs.Int("hls-variant-index", -1, "HLS variant index (optional)")
	urlValue, err := parseURLArgs(fs, args)
	if err != nil {
		return printCommandError(stderr, "download", err)
	}
	if *hlsVariantIndex < -1 {
		return printCommandError(stderr, "download", errors.New("hls variant index must be non-negative"))
	}
	taskID, err := newForegroundTaskID()
	if err != nil {
		return printCommandError(stderr, "download", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	progress := newProgressPrinter(stderr)
	result, err := media.DefaultRunner().Download(ctx, taskID, app.CreateTaskRequest{
		URL: urlValue, Referer: *referer, Origin: *origin, UserAgent: *userAgent, Cookie: *cookie,
		OutputDir: *outputDir, OutputName: *outputName, Format: *format,
		HLSVariantIndex: optionalVariantIndex(*hlsVariantIndex),
	}, progress.report)
	progress.finish()
	if err != nil {
		return printCommandError(stderr, "download", err)
	}
	return encodeResult(stdout, result)
}

func runTasks(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("tasks", stderr)
	serverValue := fs.String("server", defaultServerURL, "local riflo server URL")
	status := fs.String("status", "", "filter by status")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		return printCommandError(stderr, "tasks", fmt.Errorf("unexpected argument %q", fs.Arg(0)))
	}
	if strings.TrimSpace(*status) != "" && !app.TaskStatus(*status).Valid() {
		return printCommandError(stderr, "tasks", app.Invalid("unknown task status"))
	}
	client, err := newLocalAPIClient(*serverValue)
	if err != nil {
		return printCommandError(stderr, "tasks", err)
	}
	items, err := client.ListTasks(context.Background(), app.TaskFilter{Status: app.TaskStatus(*status)})
	if err != nil {
		return printCommandError(stderr, "tasks", err)
	}
	return encodeResult(stdout, map[string]any{"tasks": items})
}

func runTask(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("task", stderr)
	serverValue := fs.String("server", defaultServerURL, "local riflo server URL")
	taskID, err := parseTaskIDArgs(fs, args)
	if err != nil {
		return printCommandError(stderr, "task", err)
	}
	if err := validateTaskID(taskID); err != nil {
		return printCommandError(stderr, "task", err)
	}
	client, err := newLocalAPIClient(*serverValue)
	if err != nil {
		return printCommandError(stderr, "task", err)
	}
	task, err := client.GetTask(context.Background(), taskID)
	if err != nil {
		return printCommandError(stderr, "task", err)
	}
	return encodeResult(stdout, task)
}

func runCancel(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("cancel", stderr)
	serverValue := fs.String("server", defaultServerURL, "local riflo server URL")
	taskID, err := parseTaskIDArgs(fs, args)
	if err != nil {
		return printCommandError(stderr, "cancel", err)
	}
	if err := validateTaskID(taskID); err != nil {
		return printCommandError(stderr, "cancel", err)
	}
	client, err := newLocalAPIClient(*serverValue)
	if err != nil {
		return printCommandError(stderr, "cancel", err)
	}
	task, err := client.CancelTask(context.Background(), taskID)
	if err != nil {
		return printCommandError(stderr, "cancel", err)
	}
	return encodeResult(stdout, task)
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("riflo "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func parseURLArgs(fs *flag.FlagSet, args []string) (string, error) {
	positional := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		positional = args[0]
		args = args[1:]
	}
	hadPositional := positional != ""
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if positional == "" {
		if fs.NArg() == 0 {
			return "", fmt.Errorf("a media URL is required")
		}
		positional = fs.Arg(0)
	}
	if fs.NArg() > 1 || (hadPositional && fs.NArg() > 0) {
		return "", fmt.Errorf("exactly one media URL is required")
	}
	positional = strings.TrimSpace(positional)
	if positional == "" {
		return "", fmt.Errorf("a media URL is required")
	}
	return positional, nil
}

func parseTaskIDArgs(fs *flag.FlagSet, args []string) (string, error) {
	positional := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		positional = args[0]
		args = args[1:]
	}
	hadPositional := positional != ""
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if positional == "" {
		if fs.NArg() == 0 {
			return "", fmt.Errorf("one task ID is required")
		}
		positional = fs.Arg(0)
	}
	if fs.NArg() > 1 || (hadPositional && fs.NArg() > 0) {
		return "", fmt.Errorf("exactly one task ID is required")
	}
	return strings.TrimSpace(positional), nil
}

func newForegroundTaskID() (string, error) {
	var random [8]byte
	if _, err := cryptorand.Read(random[:]); err != nil {
		return "", fmt.Errorf("create task id: %w", err)
	}
	return "cli-" + hex.EncodeToString(random[:]), nil
}

func optionalVariantIndex(value int) *int {
	if value < 0 {
		return nil
	}
	return &value
}

type progressPrinter struct {
	writer    io.Writer
	lastAt    time.Time
	lastFrac  float64
	lastBytes int64
	printed   bool
}

func newProgressPrinter(writer io.Writer) *progressPrinter {
	return &progressPrinter{writer: writer, lastFrac: media.UnknownFraction}
}

func (p *progressPrinter) report(value media.Progress) {
	if p == nil || p.writer == nil {
		return
	}
	now := time.Now()
	terminal := value.Fraction >= 1
	changed := value.Fraction != p.lastFrac || value.BytesDone != p.lastBytes
	if !terminal && !changed {
		return
	}
	if !terminal && !p.lastAt.IsZero() && now.Sub(p.lastAt) < progressInterval {
		return
	}
	p.lastAt = now
	p.lastFrac = value.Fraction
	p.lastBytes = value.BytesDone
	p.printed = true
	if value.Fraction >= 0 {
		fmt.Fprintf(p.writer, "download: %3.0f%%", value.Fraction*100)
	} else if value.BytesDone > 0 {
		fmt.Fprintf(p.writer, "download: %s", formatBytes(value.BytesDone))
	} else {
		return
	}
	if value.DurationMS > 0 && value.OutTimeMS > 0 {
		fmt.Fprintf(p.writer, " (%s/%s)", formatDuration(value.OutTimeMS), formatDuration(value.DurationMS))
	}
	fmt.Fprintln(p.writer)
}

func (p *progressPrinter) finish() {
	// Progress is line-oriented so it is safe for pipes and redirected stderr.
}

func formatBytes(value int64) string {
	if value < 1024 {
		return fmt.Sprintf("%d B", value)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	amount := float64(value)
	for _, unit := range units {
		amount /= 1024
		if amount < 1024 {
			return fmt.Sprintf("%.1f %s", amount, unit)
		}
	}
	return fmt.Sprintf("%.1f PiB", amount/1024)
}

func formatDuration(milliseconds int64) string {
	if milliseconds < 0 {
		milliseconds = 0
	}
	seconds := milliseconds / 1000
	return fmt.Sprintf("%02d:%02d:%02d", seconds/3600, (seconds/60)%60, seconds%60)
}

func validateTaskID(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return app.Invalid("task ID is required")
	}
	if strings.ContainsAny(value, "/\\?#\r\n\x00") {
		return app.Invalid("task ID is invalid")
	}
	return nil
}

func encodeResult(w io.Writer, value any) int {
	if err := json.NewEncoder(w).Encode(value); err != nil {
		return 1
	}
	return 0
}

func printCommandError(w io.Writer, command string, err error) int {
	if err == nil {
		return 0
	}
	code := 1
	var appErr *app.Error
	if errors.As(err, &appErr) && appErr != nil {
		switch {
		case appErr.Code == "canceled" || appErr.Status == http.StatusRequestTimeout:
			code = 130
		case appErr.Status == http.StatusBadRequest:
			code = 2
		}
	}
	if errors.Is(err, media.ErrInvalidURL) || errors.Is(err, media.ErrUnsupportedFormat) {
		code = 2
	}
	if errors.Is(err, media.ErrAccessDenied) {
		err = errors.New("media server denied access; try adding Referer or Cookie")
	}
	if errors.Is(err, context.Canceled) {
		code = 130
		err = errors.New("operation canceled (interrupt received)")
	} else if errors.Is(err, context.DeadlineExceeded) {
		code = 1
		err = errors.New("operation timed out")
	}
	fmt.Fprintf(w, "riflo %s: %v\n", command, err)
	return code
}
