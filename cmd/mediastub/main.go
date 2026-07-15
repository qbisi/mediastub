package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/qbisi/mediastub/core"
	"github.com/qbisi/mediastub/internal/sdnotify"
	"github.com/qbisi/mediastub/mountfs"
	"github.com/qbisi/mediastub/origin"
	"github.com/qbisi/mediastub/pathfilter"
	"github.com/qbisi/mediastub/syncer"
)

const defaultIncludes = pathfilter.DefaultIncludes

const (
	webDAVUserEnv     = "WEBDAV_USER"
	webDAVPasswordEnv = "WEBDAV_PASSWORD"
	webDAVTokenEnv    = "WEBDAV_TOKEN"
)

type byteSize int64

func (s *byteSize) String() string {
	value := int64(*s)
	for _, unit := range []struct {
		name string
		size int64
	}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}} {
		if value >= unit.size && value%unit.size == 0 {
			return fmt.Sprintf("%d%s", value/unit.size, unit.name)
		}
	}
	return strconv.FormatInt(value, 10)
}

func (s *byteSize) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("empty size")
	}
	lower := strings.ToLower(value)
	multiplier := int64(1)
	for _, suffix := range []struct {
		name       string
		multiplier int64
	}{
		{"kib", 1 << 10}, {"kb", 1 << 10}, {"k", 1 << 10},
		{"mib", 1 << 20}, {"mb", 1 << 20}, {"m", 1 << 20},
		{"gib", 1 << 30}, {"gb", 1 << 30}, {"g", 1 << 30},
	} {
		if strings.HasSuffix(lower, suffix.name) {
			value = strings.TrimSpace(value[:len(value)-len(suffix.name)])
			multiplier = suffix.multiplier
			break
		}
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n <= 0 || n > (1<<63-1)/multiplier {
		return fmt.Errorf("invalid positive size %q", value)
	}
	*s = byteSize(n * multiplier)
	return nil
}

type mountOptions struct {
	include      string
	maxRead      byteSize
	maxRequests  int
	windowSize   byteSize
	onError      string
	cacheEntries int
	stubProcess  string
	stubUID      string
	stubGID      string
	allowOther   bool
	logLevel     string
	attrTTL      time.Duration
}

type syncOptions struct {
	include      string
	pollInterval time.Duration
	settleTime   time.Duration
	stateDir     string
	logLevel     string
	daemon       bool
}

func (o syncOptions) validate() error {
	if o.stateDir == "" {
		return errors.New("--state-dir is required")
	}
	if !filepath.IsAbs(o.stateDir) {
		return errors.New("--state-dir must be absolute")
	}
	if o.pollInterval <= 0 {
		return errors.New("--poll-interval must be positive")
	}
	if o.settleTime <= 0 {
		return errors.New("--settle-time must be positive")
	}
	if o.logLevel != "info" && o.logLevel != "verbose" && o.logLevel != "debug" {
		return errors.New("--log-level must be info, verbose or debug")
	}
	_, err := pathfilter.New(includes(o.include))
	return err
}

func (o mountOptions) validate() error {
	if o.maxRequests <= 0 {
		return errors.New("--probe-max-requests must be positive")
	}
	if o.windowSize > o.maxRead {
		return errors.New("--probe-window-size must not exceed --probe-max-read")
	}
	if o.cacheEntries <= 0 {
		return errors.New("--plan-cache-entries must be positive")
	}
	if o.attrTTL <= 0 {
		return errors.New("--attr-ttl must be positive")
	}
	if _, err := mountfs.ParseLogLevel(o.logLevel); err != nil {
		return fmt.Errorf("--log-level: %w", err)
	}
	patterns := pathfilter.ParseCommaSeparated(o.stubProcess)
	for _, pattern := range patterns {
		if _, err := pathfilter.New([]string{pattern}); err != nil {
			return fmt.Errorf("invalid --stub-process pattern %q: %w", pattern, err)
		}
	}
	uids, err := numericIDs(o.stubUID, "--stub-uid")
	if err != nil {
		return err
	}
	gids, err := numericIDs(o.stubGID, "--stub-gid")
	if err != nil {
		return err
	}
	if len(patterns) == 0 && len(uids) == 0 && len(gids) == 0 {
		return errors.New("at least one of --stub-process, --stub-uid or --stub-gid is required")
	}
	return nil
}

func rootUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  mediastub mount [options] REMOTE MOUNTPOINT")
	fmt.Fprintln(w, "  mediastub sync [options] REMOTE LOCAL_DIRECTORY")
	fmt.Fprintln(w, "Options may appear before or after positional arguments.")
	fmt.Fprintln(w)
	printRemoteHelp(w)
	fmt.Fprintln(w)
	fmt.Fprintln(w, `Run "mediastub mount --help" or "mediastub sync --help" for command options.`)
}

func parseSync(args []string, output io.Writer) (syncOptions, string, string, error) {
	var opts syncOptions
	flags := flag.NewFlagSet("mediastub sync", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&opts.include, "include", defaultIncludes, "comma-separated path.Match patterns identifying media files")
	flags.DurationVar(&opts.pollInterval, "poll-interval", 5*time.Minute, "interval between complete remote scans")
	flags.DurationVar(&opts.settleTime, "settle-time", 3*time.Second, "time a local media or sidecar file must remain unchanged")
	flags.StringVar(&opts.stateDir, "state-dir", "", "absolute directory for state.json and the process lock (required)")
	flags.StringVar(&opts.logLevel, "log-level", "info", "logging detail: info, verbose or debug")
	flags.BoolVar(&opts.daemon, "daemon", false, "continue applying the initial plan, then watch local changes and poll the remote")
	flags.Usage = func() {
		fmt.Fprintln(output, "Usage: mediastub sync [options] REMOTE LOCAL_DIRECTORY")
		fmt.Fprintln(output, "Options may appear before or after REMOTE LOCAL_DIRECTORY.")
		fmt.Fprintln(output)
		printRemoteHelp(output)
		fmt.Fprintln(output)
		fmt.Fprintln(output, "Options:")
		flags.PrintDefaults()
	}
	if err := flags.Parse(interspersedFlagArgs(flags, args)); err != nil {
		return opts, "", "", err
	}
	if flags.NArg() != 2 {
		flags.Usage()
		return opts, "", "", errors.New("sync requires exactly REMOTE and LOCAL_DIRECTORY")
	}
	if err := opts.validate(); err != nil {
		return opts, "", "", err
	}
	if !filepath.IsAbs(flags.Arg(1)) {
		return opts, "", "", errors.New("LOCAL_DIRECTORY must be absolute")
	}
	return opts, flags.Arg(0), flags.Arg(1), nil
}

func printRemoteHelp(w io.Writer) {
	fmt.Fprintln(w, "Remote:")
	fmt.Fprintln(w, "  file:///absolute/path")
	fmt.Fprintln(w, "  http://host:port/url-path")
	fmt.Fprintln(w, "  https://host:port/url-path")
	fmt.Fprintln(w, "  http+unix://%2Fpath%2Fto%2Fsocket/url-path")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "WebDAV environment:")
	fmt.Fprintf(w, "  %s      Basic Auth username\n", webDAVUserEnv)
	fmt.Fprintf(w, "  %s  Basic Auth password\n", webDAVPasswordEnv)
	fmt.Fprintf(w, "  %s     Bearer token (mutually exclusive with Basic Auth)\n", webDAVTokenEnv)
	fmt.Fprintln(w, "  Set both Basic variables, only the token, or none.")
}

func parseMount(args []string, output io.Writer) (mountOptions, string, string, error) {
	var opts mountOptions
	opts.maxRead = 16 << 20
	opts.windowSize = 256 << 10
	flags := flag.NewFlagSet("mediastub mount", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&opts.include, "include", defaultIncludes, "comma-separated path.Match patterns eligible for media probing")
	flags.Var(&opts.maxRead, "probe-max-read", "maximum source bytes read by one probe (supports KiB, MiB, GiB)")
	flags.IntVar(&opts.maxRequests, "probe-max-requests", 128, "maximum upstream reads made by one probe")
	flags.Var(&opts.windowSize, "probe-window-size", "size of each coalesced upstream probe read")
	flags.StringVar(&opts.onError, "on-probe-error", "passthrough", "probe failure policy: passthrough or fail")
	flags.IntVar(&opts.cacheEntries, "plan-cache-entries", 1024, "maximum number of in-memory probe plans")
	flags.StringVar(&opts.stubProcess, "stub-process", "ffprobe", "comma-separated /proc/PID/comm patterns; OR with stub UID/GID rules")
	flags.StringVar(&opts.stubUID, "stub-uid", "", "comma-separated effective UIDs; OR with comm/GID rules")
	flags.StringVar(&opts.stubGID, "stub-gid", "", "comma-separated effective GIDs; OR with comm/UID rules")
	flags.BoolVar(&opts.allowOther, "allow-other", false, "allow users other than the mounting user to access the mount")
	flags.StringVar(&opts.logLevel, "log-level", "info", "logging detail: info, verbose or debug")
	flags.DurationVar(&opts.attrTTL, "attr-ttl", time.Second, "kernel attribute, entry and negative lookup TTL")
	flags.Usage = func() {
		fmt.Fprintln(output, "Usage: mediastub mount [options] REMOTE MOUNTPOINT")
		fmt.Fprintln(output, "Options may appear before or after REMOTE MOUNTPOINT.")
		fmt.Fprintln(output)
		printRemoteHelp(output)
		fmt.Fprintln(output)
		fmt.Fprintln(output, "Options:")
		flags.PrintDefaults()
	}
	if err := flags.Parse(interspersedFlagArgs(flags, args)); err != nil {
		return opts, "", "", err
	}
	if flags.NArg() != 2 {
		flags.Usage()
		return opts, "", "", errors.New("mount requires exactly REMOTE and MOUNTPOINT")
	}
	if err := opts.validate(); err != nil {
		return opts, "", "", err
	}
	return opts, flags.Arg(0), flags.Arg(1), nil
}

// interspersedFlagArgs moves recognized flags before positional arguments so
// the standard flag package accepts options on either side of REMOTE and
// MOUNTPOINT. A standalone -- retains its usual end-of-options meaning.
func interspersedFlagArgs(flags *flag.FlagSet, args []string) []string {
	options := make([]string, 0, len(args))
	positionals := make([]string, 0, len(args))
	positionalOnly := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if positionalOnly {
			positionals = append(positionals, arg)
			continue
		}
		if arg == "--" {
			positionalOnly = true
			continue
		}
		if len(arg) < 2 || arg[0] != '-' || arg == "-" {
			positionals = append(positionals, arg)
			continue
		}

		options = append(options, arg)
		name := arg[1:]
		if strings.HasPrefix(name, "-") {
			name = name[1:]
		}
		name, _, hasValue := strings.Cut(name, "=")
		option := flags.Lookup(name)
		if option == nil || hasValue {
			continue
		}
		if boolean, ok := option.Value.(interface{ IsBoolFlag() bool }); ok && boolean.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			i++
			options = append(options, args[i])
		}
	}
	return append(options, positionals...)
}

func includes(value string) []string {
	return pathfilter.ParseCommaSeparated(value)
}

func numericIDs(value, option string) ([]uint32, error) {
	var ids []uint32
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		id, err := strconv.ParseUint(item, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid %s value %q: want an unsigned 32-bit integer", option, item)
		}
		ids = append(ids, uint32(id))
	}
	return ids, nil
}

func webDAVCredentials(remote string) (origin.Auth, error) {
	if !strings.HasPrefix(remote, "http://") && !strings.HasPrefix(remote, "https://") && !strings.HasPrefix(remote, "http+unix://") {
		return origin.Auth{}, nil
	}
	user, userSet := os.LookupEnv(webDAVUserEnv)
	password, passwordSet := os.LookupEnv(webDAVPasswordEnv)
	if userSet != passwordSet {
		return origin.Auth{}, fmt.Errorf("%s and %s must be set together", webDAVUserEnv, webDAVPasswordEnv)
	}
	token, tokenSet := os.LookupEnv(webDAVTokenEnv)
	if userSet && (user == "" || password == "") {
		return origin.Auth{}, errors.New("WebDAV Basic credentials must not be empty")
	}
	if tokenSet && token == "" {
		return origin.Auth{}, errors.New("WEBDAV_TOKEN must not be empty")
	}
	auth := origin.Auth{User: user, Password: password}
	if tokenSet {
		auth.BearerToken = token
	}
	if err := auth.Validate(); err != nil {
		return origin.Auth{}, err
	}
	return auth, nil
}

func mountCommand(args []string) error {
	opts, remote, mountpoint, err := parseMount(args, os.Stderr)
	if err != nil {
		return err
	}
	stubUIDs, err := numericIDs(opts.stubUID, "--stub-uid")
	if err != nil {
		return err
	}
	stubGIDs, err := numericIDs(opts.stubGID, "--stub-gid")
	if err != nil {
		return err
	}
	auth, err := webDAVCredentials(remote)
	if err != nil {
		return err
	}
	upstream, err := origin.NewRemoteWithAuth(remote, auth)
	if err != nil {
		return err
	}
	defer upstream.Close()

	logger := log.New(os.Stderr, "mediastub: ", log.LstdFlags|log.Lmicroseconds)
	logLevel, err := mountfs.ParseLogLevel(opts.logLevel)
	if err != nil {
		return err
	}
	service, err := mountfs.NewService(upstream, mountfs.Config{
		Includes: includes(opts.include),
		Budget: core.Budget{
			MaxBytes: int64(opts.maxRead), MaxRequests: opts.maxRequests, WindowSize: int(opts.windowSize),
		},
		OnError: opts.onError, CacheEntries: opts.cacheEntries, Logger: logger,
	})
	if err != nil {
		return err
	}
	server, err := mountfs.Mount(mountpoint, service, mountfs.MountOptions{
		AllowOther: opts.allowOther, LogLevel: logLevel, AttrTTL: opts.attrTTL, Logger: logger,
		StubProcesses: includes(opts.stubProcess),
		StubUIDs:      stubUIDs,
		StubGIDs:      stubGIDs,
	})
	if err != nil {
		return err
	}
	logger.Printf("mounted remote=%q mountpoint=%q", remote, mountpoint)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan struct{})
	go func() {
		server.Wait()
		close(done)
	}()
	select {
	case <-ctx.Done():
		logger.Printf("unmounting mountpoint=%q", mountpoint)
		if err := server.Unmount(); err != nil {
			return err
		}
		<-done
	case <-done:
	}
	return nil
}

func syncCommand(args []string) error {
	opts, remote, localDirectory, err := parseSync(args, os.Stderr)
	if err != nil {
		return err
	}
	auth, err := webDAVCredentials(remote)
	if err != nil {
		return err
	}
	upstream, err := origin.NewRemoteWithAuth(remote, auth)
	if err != nil {
		return err
	}
	defer upstream.Close()
	logger := log.New(os.Stderr, "mediastub: ", log.LstdFlags|log.Lmicroseconds)
	service, err := syncer.New(upstream, syncer.Config{
		Remote: remote, LocalRoot: localDirectory, StateDir: opts.stateDir,
		Includes: includes(opts.include), PollInterval: opts.pollInterval,
		SettleTime: opts.settleTime, LogLevel: opts.logLevel, Daemon: opts.daemon,
		Budget: core.DefaultBudget, Logger: logger,
	})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.Printf("synchronizing remote=%q local_directory=%q", remote, localDirectory)
	if !opts.daemon {
		return service.Run(ctx, nil)
	}
	return service.Run(ctx, func(status string) error {
		logger.Printf("ready: %s", status)
		return sdnotify.Ready(status)
	})
}

func run(args []string) error {
	if len(args) == 0 {
		rootUsage(os.Stderr)
		return errors.New("a command is required")
	}
	switch args[0] {
	case "mount":
		return mountCommand(args[1:])
	case "sync":
		return syncCommand(args[1:])
	case "help", "-h", "--help":
		rootUsage(os.Stdout)
		return nil
	default:
		rootUsage(os.Stderr)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func main() {
	if err := run(os.Args[1:]); err != nil && !errors.Is(err, flag.ErrHelp) {
		log.Printf("mediastub: %v", err)
		os.Exit(1)
	}
}
