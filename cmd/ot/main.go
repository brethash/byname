// Command ot is a household short-link service ("ot/wiki") for a Raspberry Pi
// on a tailnet.
//
//	ot serve     run the web service
//	ot snapshot  write a consistent database snapshot
//	ot backup    upload the newest snapshot to S3
//	ot export    print all links as JSON
//	ot import    load links from an export file
//	ot version   print the version
//
// Every flag can also be set with an OT_* environment variable (for example
// --hosts ⇔ OT_HOSTS), which is how the systemd units configure it.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/time/rate"

	"otterly-go/internal/app"
	"otterly-go/internal/backup"
	"otterly-go/internal/store"
	"otterly-go/internal/web"
	"otterly-go/internal/whois"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const (
	defaultDB          = "/var/lib/ot/ot.db"
	defaultSnapshotDir = "/var/lib/ot/snapshots"
	defaultThrottled   = "/sys/devices/platform/soc/soc:firmware/get_throttled"
	tailnetNets        = "100.64.0.0/10,fd7a:115c:a1e0::/48"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmds := map[string]func([]string) error{
		"serve":    serve,
		"snapshot": snapshot,
		"backup":   runBackup,
		"export":   export,
		"import":   importLinks,
		"version":  func([]string) error { fmt.Println(version); return nil },
	}
	cmd, ok := cmds[os.Args[1]]
	if !ok {
		usage()
		os.Exit(2)
	}
	if err := cmd(os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "ot %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: ot <command> [flags]

commands:
  serve     run the web service
  snapshot  write a consistent database snapshot
  backup    upload the newest snapshot to S3
  export    print all links as JSON
  import    load links from an export file
  version   print the version

Run "ot <command> -h" for flags. Every flag may also be set as OT_<NAME>
(upper case, dashes as underscores), e.g. OT_HOSTS=ot,ot.example.ts.net.
`)
}

// newFlags returns a FlagSet whose defaults come from OT_* env vars.
func newFlags(name string) *flag.FlagSet {
	return flag.NewFlagSet("ot "+name, flag.ExitOnError)
}

// parse applies OT_* environment variables for flags not given explicitly.
func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	var err error
	fs.VisitAll(func(f *flag.Flag) {
		if set[f.Name] || err != nil {
			return
		}
		env := "OT_" + strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))
		if v, ok := os.LookupEnv(env); ok {
			if e := fs.Set(f.Name, v); e != nil {
				err = fmt.Errorf("%s: %w", env, e)
			}
		}
	})
	return err
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func newLogger(level, format string) (*slog.Logger, error) {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("log level: %w", err)
	}
	opts := &slog.HandlerOptions{Level: lv}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts)), nil
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts)), nil
}

func serve(args []string) error {
	fs := newFlags("serve")
	listen := fs.String("listen", ":80", "listen address")
	dbPath := fs.String("db", defaultDB, "SQLite database path")
	hosts := fs.String("hosts", "", "comma-separated Host names to answer to; the first is shown in the UI (required, e.g. ot,ot.example.ts.net)")
	allow := fs.String("allow-cidrs", tailnetNets, "comma-separated peer networks allowed to connect")
	socket := fs.String("tailscale-socket", whois.DefaultSocket, "tailscaled LocalAPI socket for WhoIs")
	noWhois := fs.Bool("no-whois", false, "disable Tailscale WhoIs (changes are attributed by IP only)")
	snapDir := fs.String("snapshot-dir", defaultSnapshotDir, `snapshot directory ("" disables snapshots)`)
	snapKeep := fs.Int("snapshot-keep", 14, "snapshots kept locally")
	snapEvery := fs.Duration("snapshot-every", 24*time.Hour, "periodic snapshot interval")
	clickFlush := fs.Duration("click-flush", 5*time.Minute, "how often batched click counts are written")
	maxLinks := fs.Int("max-links", 10000, "maximum live links (0 = unlimited)")
	writesPerMin := fs.Float64("write-rate", 30, "changes allowed per device per minute")
	writeBurst := fs.Int("write-burst", 10, "burst of changes allowed per device")
	throttled := fs.String("throttled-path", defaultThrottled, `Raspberry Pi firmware get_throttled file ("" disables)`)
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")
	logFormat := fs.String("log-format", "text", "text or json")
	if err := parse(fs, args); err != nil {
		return err
	}

	log, err := newLogger(*logLevel, *logFormat)
	if err != nil {
		return err
	}
	hostList := splitList(*hosts)
	if len(hostList) == 0 {
		return errors.New("--hosts is required (e.g. --hosts ot,ot.example.ts.net)")
	}
	var nets []netip.Prefix
	for _, c := range splitList(*allow) {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			return fmt.Errorf("--allow-cidrs: %w", err)
		}
		nets = append(nets, p.Masked())
	}
	if *snapDir != "" {
		*snapDir = filepath.Clean(*snapDir)
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer st.Close()

	svc := app.New(st, app.Config{
		SelfHosts:     hostList,
		MaxLinks:      *maxLinks,
		ClickFlush:    *clickFlush,
		SnapshotDir:   *snapDir,
		SnapshotKeep:  *snapKeep,
		SnapshotEvery: *snapEvery,
		Logger:        log,
	})

	var who whois.Resolver = whois.Nop{}
	if !*noWhois {
		who = whois.New(*socket)
	}

	limit := rate.Limit(*writesPerMin / 60)
	if *writesPerMin <= 0 {
		limit = rate.Inf
	}
	handler, err := web.New(web.Config{
		Hosts:         hostList,
		AllowedNets:   nets,
		WriteRate:     limit,
		WriteBurst:    *writeBurst,
		ThrottledPath: *throttled,
		Version:       version,
		Logger:        log,
	}, svc, who)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	bgDone := make(chan struct{})
	go func() { svc.Run(ctx); close(bgDone) }()

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("ot serving", "version", version, "listen", *listen, "hosts", hostList,
		"allow", *allow, "whois", !*noWhois, "db", *dbPath, "snapshots", *snapDir)

	select {
	case err := <-errc:
		stop()
		<-bgDone
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		log.Warn("shutdown", "err", err)
	}
	<-bgDone // flushes click counts
	return nil
}

func snapshot(args []string) error {
	fs := newFlags("snapshot")
	dbPath := fs.String("db", defaultDB, "SQLite database path")
	dir := fs.String("snapshot-dir", defaultSnapshotDir, "snapshot directory")
	keep := fs.Int("snapshot-keep", 14, "snapshots kept locally")
	if err := parse(fs, args); err != nil {
		return err
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	p, err := app.SnapshotTo(context.Background(), st, *dir, *keep)
	if err != nil {
		return err
	}
	fmt.Println(p)
	return nil
}

func runBackup(args []string) error {
	fs := newFlags("backup")
	dir := fs.String("snapshot-dir", defaultSnapshotDir, "snapshot directory to upload from")
	bucket := fs.String("bucket", "", "S3 bucket (required)")
	prefix := fs.String("prefix", "ot/", "key prefix in the bucket")
	region := fs.String("region", "", "AWS region (default from the AWS config chain)")
	state := fs.String("state-file", "/var/lib/ot-backup/last-upload", "file recording the last uploaded snapshot")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *bucket == "" {
		return errors.New("--bucket is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	up, err := backup.NewS3(ctx, *bucket, *region)
	if err != nil {
		return err
	}
	res, err := backup.Run(ctx, up, backup.Options{SnapshotDir: *dir, Prefix: *prefix, StateFile: *state})
	if err != nil {
		return err
	}
	if res.Skipped {
		fmt.Printf("up to date: %s already uploaded\n", res.Snapshot)
	} else {
		fmt.Printf("uploaded %s to s3://%s/%s (%d bytes)\n", res.Snapshot, *bucket, res.Key, res.Bytes)
	}
	return nil
}

func export(args []string) error {
	fs := newFlags("export")
	dbPath := fs.String("db", defaultDB, "SQLite database path")
	out := fs.String("out", "-", `output file ("-" for stdout)`)
	if err := parse(fs, args); err != nil {
		return err
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	links, err := st.List(context.Background(), store.ListOptions{})
	if err != nil {
		return err
	}
	var w io.Writer = os.Stdout
	if *out != "-" {
		f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(web.BuildExport(links, time.Now()))
}

func importLinks(args []string) error {
	fs := newFlags("import")
	dbPath := fs.String("db", defaultDB, "SQLite database path")
	hosts := fs.String("hosts", "", "the service's host names (rejects self-pointing links), as for serve")
	overwrite := fs.Bool("overwrite", false, "update links that already exist instead of skipping them")
	dryRun := fs.Bool("dry-run", false, "validate only; change nothing")
	if err := parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New(`usage: ot import [flags] <file.json | ->`)
	}
	var r io.Reader = os.Stdin
	if p := fs.Arg(0); p != "-" {
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		r = f
	}
	var ex web.Export
	if err := json.NewDecoder(io.LimitReader(r, 64<<20)).Decode(&ex); err != nil {
		return fmt.Errorf("read export: %w", err)
	}
	if ex.Format != web.ExportFormat {
		return fmt.Errorf("not an ot export (format %q)", ex.Format)
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	svc := app.New(st, app.Config{SelfHosts: splitList(*hosts)})

	var (
		valid []store.Fields
		bad   int
	)
	for _, l := range ex.Links {
		f, err := svc.Validate(app.Input{Name: l.Name, Target: l.Target, Description: l.Description})
		if err != nil {
			fmt.Fprintf(os.Stderr, "skip %q: %v\n", l.Name, err)
			bad++
			continue
		}
		valid = append(valid, f)
	}
	if *dryRun {
		fmt.Printf("dry run: %d valid, %d invalid\n", len(valid), bad)
		return nil
	}
	res, err := st.Import(context.Background(), valid, store.Actor{Login: "import", IP: "local"}, *overwrite)
	if err != nil {
		return err
	}
	for _, e := range res.Errors {
		fmt.Fprintln(os.Stderr, "error:", e)
	}
	fmt.Printf("created %d, updated %d, skipped %d, invalid %d, failed %d\n",
		res.Created, res.Updated, res.Skipped, bad, len(res.Errors))
	return nil
}
