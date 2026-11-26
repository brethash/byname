// Command byname is a household short-link service ("me/wiki") for a Raspberry Pi
// on a tailnet.
//
//	byname serve     run the web service
//	byname snapshot  write a consistent database snapshot
//	byname backup    upload the newest snapshot to S3
//	byname export    print all links as JSON
//	byname import    load links from an export file
//	byname sessions  list or revoke sign-ins on the public address
//	byname version   print the version
//
// Every flag can also be set with an BYNAME_* environment variable (for example
// --hosts ⇔ BYNAME_HOSTS), which is how the systemd units configure it.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	stdlog "log"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/acme/autocert"
	"golang.org/x/time/rate"

	"byname/internal/app"
	"byname/internal/auth"
	"byname/internal/backup"
	"byname/internal/store"
	"byname/internal/web"
	"byname/internal/whois"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const (
	defaultDB          = "/var/lib/byname/byname.db"
	defaultSnapshotDir = "/var/lib/byname/snapshots"
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
		"sessions": sessions,
		"version":  func([]string) error { fmt.Println(version); return nil },
	}
	cmd, ok := cmds[os.Args[1]]
	if !ok {
		usage()
		os.Exit(2)
	}
	if err := cmd(os.Args[2:]); err != nil {
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "byname %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: byname <command> [flags]

commands:
  serve     run the web service
  snapshot  write a consistent database snapshot
  backup    upload the newest snapshot to S3
  export    print all links as JSON
  import    load links from an export file
  sessions  list or revoke sign-ins on the public address
  version   print the version

Run "byname <command> -h" for flags. Every flag may also be set as BYNAME_<NAME>
(upper case, dashes as underscores), e.g. BYNAME_HOSTS=me,me.example.ts.net.
`)
}

// newFlags returns a FlagSet whose defaults come from BYNAME_* env vars.
func newFlags(name string) *flag.FlagSet {
	return flag.NewFlagSet("byname "+name, flag.ExitOnError)
}

// parse applies BYNAME_* environment variables for flags not given explicitly.
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
		env := "BYNAME_" + strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))
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
	hosts := fs.String("hosts", "", "comma-separated Host names to answer to; the first is shown in the UI (required, e.g. me,me.example.ts.net)")
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
	writeDaily := fs.Int("write-daily", 300, "changes allowed per device per 24 hours (0 = unlimited)")
	throttled := fs.String("throttled-path", defaultThrottled, `Raspberry Pi firmware get_throttled file ("" disables)`)
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")
	logFormat := fs.String("log-format", "text", "text or json")
	publicURL := fs.String("public-url", "", "public https:// address, e.g. https://me.example.com (served directly) or https://NAME.TAILNET.ts.net (through Tailscale Funnel); empty = no public address")
	publicListen := fs.String("public-listen", "", `where the public address is served: ":443" to terminate TLS here with a Let's Encrypt certificate, or "unix:/path" for Tailscale Serve/Funnel to proxy to (default: unix:/run/byname/public.sock for a .ts.net address, :443 otherwise)`)
	acmeDir := fs.String("acme-dir", "/var/lib/byname/acme", "where Let's Encrypt account keys and certificates are kept (direct public address only)")
	acmeEmail := fs.String("acme-email", "", "optional contact address for Let's Encrypt expiry notices (direct public address only)")
	maxConns := fs.Int("max-public-conns", 256, "concurrent internet connections per internet-facing listener (tailnet, LAN and localhost peers don't count)")
	maxPerPeer := fs.Int("max-conns-per-ip", 32, "concurrent internet connections per IPv4 address or IPv6 /64")
	sessionTTL := fs.Duration("session-ttl", 30*24*time.Hour, "how long a Google or Slack sign-in lasts")
	googleID := fs.String("google-client-id", "", "Google OAuth client ID (enables Sign in with Google)")
	googleSecret := fs.String("google-client-secret-file", "", "file holding the Google client secret (default: systemd credential byname.google-client-secret)")
	googleAllow := fs.String("google-allow", "", "comma-separated Google accounts (alice@gmail.com) and Workspace domains (example.com) that may sign in")
	slackID := fs.String("slack-client-id", "", "Slack app client ID (enables Sign in with Slack)")
	slackSecret := fs.String("slack-client-secret-file", "", "file holding the Slack client secret (default: systemd credential byname.slack-client-secret)")
	slackTeams := fs.String("slack-teams", "", "comma-separated Slack workspace IDs (T0123ABCD) whose members may sign in")
	if err := parse(fs, args); err != nil {
		return err
	}

	log, err := newLogger(*logLevel, *logFormat)
	if err != nil {
		return err
	}
	hostList := splitList(*hosts)
	if len(hostList) == 0 {
		return errors.New("--hosts is required (e.g. --hosts me,me.example.ts.net)")
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

	pub, err := parsePublic(*publicURL, *publicListen)
	if err != nil {
		return err
	}
	*publicListen = pub.listen

	st, err := store.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer st.Close()

	svc := app.New(st, app.Config{
		SelfHosts:     selfHosts(hostList, pub.host),
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

	var providers []*auth.Provider
	if *googleID != "" {
		secret, err := readSecret(*googleSecret, "byname.google-client-secret")
		if err != nil {
			return fmt.Errorf("google: %w", err)
		}
		p, err := auth.Google(auth.GoogleConfig{ClientID: *googleID, ClientSecret: secret, Allow: splitList(*googleAllow)})
		if err != nil {
			return err
		}
		providers = append(providers, p)
	} else if *googleAllow != "" {
		return errors.New("--google-allow is set but --google-client-id is not")
	}
	if *slackID != "" {
		secret, err := readSecret(*slackSecret, "byname.slack-client-secret")
		if err != nil {
			return fmt.Errorf("slack: %w", err)
		}
		p, err := auth.Slack(auth.SlackConfig{ClientID: *slackID, ClientSecret: secret, Teams: splitList(*slackTeams)})
		if err != nil {
			return err
		}
		providers = append(providers, p)
	} else if *slackTeams != "" {
		return errors.New("--slack-teams is set but --slack-client-id is not")
	}

	limit := rate.Limit(*writesPerMin / 60)
	if *writesPerMin <= 0 {
		limit = rate.Inf
	}
	webCfg := web.Config{
		Hosts:         hostList,
		AllowedNets:   nets,
		WriteRate:     limit,
		WriteBurst:    *writeBurst,
		WriteDaily:    *writeDaily,
		DataDir:       filepath.Dir(*dbPath),
		ThrottledPath: *throttled,
		Version:       version,
		Logger:        log,
		PublicURL:     *publicURL,
		Providers:     providers,
		SessionTTL:    *sessionTTL,
	}
	webCfg.PublicBehindTailscale = pub.behindTailscale
	handler, err := web.New(webCfg, svc, who)
	if err != nil {
		return err
	}

	newServer := func(h http.Handler) *http.Server {
		return &http.Server{
			Handler:           h,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			// Covers a sign-in callback, which waits up to 20 s for the
			// provider's token endpoint.
			WriteTimeout:   30 * time.Second,
			IdleTimeout:    60 * time.Second,
			MaxHeaderBytes: 16 << 10,
			ErrorLog:       slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		}
	}
	srv := newServer(handler)
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	var pubSrv *http.Server
	var pubLn net.Listener
	pubTLS := false
	switch pubHandler := handler.Public(); {
	case pubHandler == nil:
	case webCfg.PublicBehindTailscale:
		if pubLn, err = listenUnix(*publicListen); err != nil {
			return fmt.Errorf("--public-listen: %w", err)
		}
		pubSrv = newServer(pubHandler)
	default:
		// Serve the public domain directly. Certificates come from Let's
		// Encrypt, only ever for the public name (HostWhitelist), through the
		// TLS-ALPN or HTTP challenge. The plain-HTTP listener becomes
		// internet-facing too: it answers the HTTP challenge and redirects
		// everyone but the private listener's own peers to https.
		if err := os.MkdirAll(*acmeDir, 0o700); err != nil {
			return fmt.Errorf("--acme-dir: %w", err)
		}
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(pub.host),
			Cache:      autocert.DirCache(*acmeDir),
			Email:      *acmeEmail,
		}
		tcp, err := net.Listen("tcp", *publicListen)
		if err != nil {
			return fmt.Errorf("--public-listen: %w", err)
		}
		pubLn = guard(tcp, handler.Trusted, *maxConns, *maxPerPeer, log)
		pubSrv = newServer(pubHandler)
		pubSrv.TLSConfig = m.TLSConfig()
		pubSrv.TLSConfig.MinVersion = tls.VersionTLS12
		// Internet scanners cause a steady trickle of TLS handshake errors;
		// keep those out of the journal unless debugging, but not the rest.
		pubSrv.ErrorLog = stdlog.New(serverLogWriter{log}, "", 0)
		pubTLS = true
		srv.Handler = m.HTTPHandler(handler.PrivateOrRedirect())
		ln = guard(ln, handler.Trusted, *maxConns, *maxPerPeer, log)
	}

	// Background work (click flushing, snapshots) has its own context: it
	// stops only after both servers have finished their last requests, so
	// clicks counted during shutdown are flushed too.
	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	runCtx, stopRun := context.WithCancel(context.Background())
	defer stopRun()
	bgDone := make(chan struct{})
	go func() { svc.Run(runCtx); close(bgDone) }()

	servers := []*http.Server{srv}
	errc := make(chan error, 2)
	go func() { errc <- srv.Serve(ln) }()
	switch {
	case pubTLS:
		go func() { errc <- pubSrv.ServeTLS(pubLn, "", "") }()
	case pubSrv != nil:
		go func() { errc <- pubSrv.Serve(pubLn) }()
	}
	if pubSrv != nil {
		servers = append(servers, pubSrv)
	}
	var names []string
	for _, p := range providers {
		names = append(names, p.ID())
	}
	log.Info("byname serving", "version", version, "listen", *listen, "hosts", hostList,
		"allow", *allow, "whois", !*noWhois, "db", *dbPath, "snapshots", *snapDir,
		"public", *publicURL, "public_listen", *publicListen, "sign_in", names)

	var serveErr error
	select {
	case serveErr = <-errc:
		log.Error("server stopped", "err", serveErr)
	case <-sigCtx.Done():
	}
	// From here a second Ctrl-C (or SIGTERM) ends the process at once.
	stop()
	log.Info("shutting down")
	stopServing(log, shutdownTimeout, servers, stopRun, bgDone)
	return serveErr
}

const shutdownTimeout = 5 * time.Second

// stopServing shuts the servers down together, letting requests in flight
// finish for up to timeout, and only then stops background work (stopRun)
// and waits for it (done), which flushes the click counts those last
// requests added.
func stopServing(log *slog.Logger, timeout time.Duration, servers []*http.Server, stopRun func(), done <-chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var wg sync.WaitGroup
	for _, s := range servers {
		wg.Go(func() {
			if err := s.Shutdown(ctx); err != nil {
				log.Warn("shutdown", "err", err)
			}
		})
	}
	wg.Wait()
	stopRun()
	<-done
}

// publicAddr is where and how the public address is served.
type publicAddr struct {
	host            string // lower case, no port or trailing dot; "" when off
	listen          string // ":443" or "unix:/path"
	behindTailscale bool   // served to Tailscale Funnel over a unix socket
}

// parsePublic reads --public-url and --public-listen. web.New checks the
// URL's shape in detail; this decides only where it is served.
func parsePublic(publicURL, listen string) (publicAddr, error) {
	if publicURL == "" {
		return publicAddr{listen: listen}, nil
	}
	u, err := url.Parse(publicURL)
	if err != nil {
		return publicAddr{}, fmt.Errorf("--public-url: %w", err)
	}
	p := publicAddr{host: strings.TrimSuffix(strings.ToLower(u.Hostname()), "."), listen: listen}
	if p.listen == "" {
		p.listen = ":443"
		if strings.HasSuffix(p.host, ".ts.net") {
			p.listen = "unix:/run/byname/public.sock"
		}
	}
	p.behindTailscale = strings.HasPrefix(p.listen, "unix:")
	if !p.behindTailscale && u.Port() != "" && u.Port() != "443" {
		return publicAddr{}, errors.New("--public-url: a directly served address must use the default https port")
	}
	return p, nil
}

// selfHosts are the names links must not point at, because the link would
// redirect to itself: every --hosts name and the public address's. In
// --public-domain mode the public name is not among --hosts.
func selfHosts(hosts []string, publicHost string) []string {
	out := append([]string(nil), hosts...)
	if publicHost != "" {
		out = append(out, publicHost)
	}
	return out
}

// listenUnix listens on "unix:/path", owner-only. Only tailscaled (root) and
// byname itself may connect: the public handler trusts the forwarding headers it
// receives there. systemd's RuntimeDirectory (mode 0700) guards the directory
// too.
func listenUnix(addr string) (net.Listener, error) {
	path, ok := strings.CutPrefix(addr, "unix:")
	if !ok || !filepath.IsAbs(path) {
		return nil, fmt.Errorf("%q: want unix:/absolute/path", addr)
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode().Type() != os.ModeSocket {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		os.Remove(path) // left over from a crash
	}
	old := syscall.Umask(0o177)
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// readSecret reads a client secret from path, or from the systemd credential
// named cred when path is empty. Secrets are never taken from flags or the
// environment, where they would show up in process listings and unit files.
func readSecret(path, cred string) (string, error) {
	if path == "" {
		dir := os.Getenv("CREDENTIALS_DIRECTORY")
		if dir == "" {
			return "", fmt.Errorf("no secret: put it in /etc/credstore/%s (or pass a --*-client-secret-file)", cred)
		}
		path = filepath.Join(dir, cred)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("client secret: %w", err)
	}
	secret := strings.TrimSpace(string(b))
	if secret == "" {
		return "", fmt.Errorf("client secret file %s is empty", path)
	}
	return secret, nil
}

func sessions(args []string) error {
	fs := newFlags("sessions")
	dbPath := fs.String("db", defaultDB, "SQLite database path")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: byname sessions [--db PATH]                     list live sign-ins
       byname sessions [--db PATH] revoke EMAIL         sign EMAIL out everywhere
       byname sessions [--db PATH] revoke --all         sign everyone out
`)
	}
	if err := parse(fs, args); err != nil {
		return err
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()
	switch rest := fs.Args(); {
	case len(rest) == 0:
		list, err := st.Sessions(ctx)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Println("no one is signed in")
			return nil
		}
		for _, x := range list {
			fmt.Printf("%-32s %-7s %-12s from %-39s since %s, until %s\n", x.Email, x.Provider, x.Org, x.IP,
				x.Created.Local().Format("2006-01-02 15:04"), x.Expires.Local().Format("2006-01-02"))
		}
		return nil
	case len(rest) == 2 && rest[0] == "revoke":
		var n int64
		if rest[1] == "--all" {
			n, err = st.RevokeAllSessions(ctx)
		} else if email := strings.ToLower(strings.TrimSpace(rest[1])); email == "" {
			return errors.New(`revoke needs an email address, or --all to sign everyone out`)
		} else {
			n, err = st.RevokeSessions(ctx, email)
		}
		if err != nil {
			return err
		}
		fmt.Printf("revoked %d session(s)\n", n)
		return nil
	}
	fs.Usage()
	return errUsage
}

// errUsage tells main that a command has printed its usage: exit 2, with no
// further message.
var errUsage = errors.New("usage")

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
	prefix := fs.String("prefix", "byname/", "key prefix in the bucket")
	region := fs.String("region", "", "AWS region (default from the AWS config chain)")
	state := fs.String("state-file", "/var/lib/byname-backup/last-upload", "file recording the last uploaded snapshot")
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
	switch {
	case res.Existed:
		fmt.Printf("up to date: %s is already in s3://%s/\n", res.Snapshot, *bucket)
	case res.Skipped:
		fmt.Printf("up to date: %s already uploaded\n", res.Snapshot)
	default:
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
	publicURL := fs.String("public-url", "", "the service's public address, as for serve (rejects links pointing at it)")
	overwrite := fs.Bool("overwrite", false, "update links that already exist instead of skipping them")
	dryRun := fs.Bool("dry-run", false, "validate only; change nothing")
	if err := parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New(`usage: byname import [flags] <file.json | ->`)
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
		return fmt.Errorf("not an byname export (format %q)", ex.Format)
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	pub, err := parsePublic(*publicURL, ":443")
	if err != nil {
		return err
	}
	svc := app.New(st, app.Config{SelfHosts: selfHosts(splitList(*hosts), pub.host)})

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
