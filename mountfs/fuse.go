package mountfs

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/qbisi/mediastub/origin"
)

// MountOptions controls the FUSE projection.
type MountOptions struct {
	AllowOther bool
	LogLevel   LogLevel
	AttrTTL    time.Duration
	Logger     *log.Logger
	// StubProcesses contains path.Match patterns applied to /proc/PID/comm.
	StubProcesses []string
	// StubUIDs and StubGIDs match the effective IDs supplied by FUSE. UID,
	// effective GID and comm rules are combined with OR semantics. Leaving all
	// three slices nil preserves the library's historical all-process behavior.
	StubUIDs []uint32
	StubGIDs []uint32
}

// LogLevel controls access and FUSE protocol logging.
type LogLevel uint8

const (
	// LogLevelInfo logs opens of paths matched by the include policy.
	LogLevelInfo LogLevel = iota
	// LogLevelVerbose logs every file open.
	LogLevelVerbose
	// LogLevelDebug logs every file open and the go-fuse protocol.
	LogLevelDebug
)

// ParseLogLevel parses a command-line log level.
func ParseLogLevel(value string) (LogLevel, error) {
	switch value {
	case "info":
		return LogLevelInfo, nil
	case "verbose":
		return LogLevelVerbose, nil
	case "debug":
		return LogLevelDebug, nil
	default:
		return 0, fmt.Errorf("invalid log level %q: want info, verbose or debug", value)
	}
}

func (l LogLevel) logAllAccesses() bool { return l >= LogLevelVerbose }
func (l LogLevel) debugFUSE() bool      { return l >= LogLevelDebug }

// Mount mounts service at mountpoint and waits until the kernel accepts it.
func Mount(mountpoint string, service *Service, options MountOptions) (*fuse.Server, error) {
	if options.AttrTTL <= 0 {
		options.AttrTTL = time.Second
	}
	processes, err := newProcessMatcher(options.StubProcesses, options.StubUIDs, options.StubGIDs)
	if err != nil {
		return nil, err
	}
	root := &node{
		service: service, path: ".", attrTTL: options.AttrTTL, logger: options.Logger,
		processes: processes, logLevel: options.LogLevel,
	}
	mountOptions := fuse.MountOptions{
		Name: "mediastub", FsName: "mediastub", Debug: options.LogLevel.debugFUSE(),
		Options: []string{"ro"},
	}
	if options.AllowOther {
		mountOptions.Options = append(mountOptions.Options, "allow_other")
	}
	return fs.Mount(mountpoint, root, &fs.Options{
		MountOptions:    mountOptions,
		EntryTimeout:    &options.AttrTTL,
		AttrTimeout:     &options.AttrTTL,
		NegativeTimeout: &options.AttrTTL,
	})
}

type node struct {
	fs.Inode
	service   *Service
	path      string
	attrTTL   time.Duration
	logger    *log.Logger
	processes *processMatcher
	logLevel  LogLevel

	mu            sync.Mutex
	entry         *origin.Entry
	entryDeadline time.Time
	children      map[string]cachedEntry
}

type cachedEntry struct {
	entry    origin.Entry
	deadline time.Time
}

type processMatcher struct {
	patterns []string
	uids     map[uint32]struct{}
	gids     map[uint32]struct{}
	procRoot string
}

type callerInfo struct {
	pid  uint32
	uid  uint32
	gid  uint32
	comm string
}

func newProcessMatcher(patterns []string, uids, gids []uint32) (*processMatcher, error) {
	if patterns == nil && uids == nil && gids == nil {
		patterns = []string{"*"}
	}
	for _, pattern := range patterns {
		if pattern == "" {
			return nil, errors.New("stub process pattern must not be empty")
		}
		if _, err := path.Match(pattern, ""); err != nil {
			return nil, fmt.Errorf("invalid stub process pattern %q: %w", pattern, err)
		}
	}
	m := &processMatcher{
		patterns: append([]string(nil), patterns...),
		uids:     make(map[uint32]struct{}, len(uids)),
		gids:     make(map[uint32]struct{}, len(gids)),
		procRoot: "/proc",
	}
	for _, uid := range uids {
		m.uids[uid] = struct{}{}
	}
	for _, gid := range gids {
		m.gids[gid] = struct{}{}
	}
	return m, nil
}

func (m *processMatcher) match(ctx context.Context) (matched bool, info callerInfo, err error) {
	caller, ok := fuse.FromContext(ctx)
	if !ok {
		return false, info, errors.New("FUSE caller identity is unavailable")
	}
	info.pid = caller.Pid
	info.uid = caller.Uid
	info.gid = caller.Gid
	if _, ok := m.uids[caller.Uid]; ok {
		return true, info, nil
	}
	if _, ok := m.gids[caller.Gid]; ok {
		return true, info, nil
	}
	if len(m.patterns) == 0 {
		return false, info, nil
	}
	if caller.Pid == 0 {
		return false, info, errors.New("FUSE caller PID is unavailable for comm matching")
	}
	data, err := os.ReadFile(filepath.Join(m.procRoot, strconv.FormatUint(uint64(caller.Pid), 10), "comm"))
	if err != nil {
		return false, info, err
	}
	info.comm = strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	for _, pattern := range m.patterns {
		matched, err := path.Match(pattern, info.comm)
		if err != nil {
			return false, info, err
		}
		if matched {
			return true, info, nil
		}
	}
	return false, info, nil
}

func (n *node) cachedStat() (origin.Entry, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.entry == nil || time.Now().After(n.entryDeadline) {
		return origin.Entry{}, false
	}
	return *n.entry, true
}

func (n *node) cacheStat(entry origin.Entry, deadline time.Time) {
	n.mu.Lock()
	n.entry = &entry
	n.entryDeadline = deadline
	n.mu.Unlock()
}

func (n *node) cachedChild(name string) (origin.Entry, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	cached, ok := n.children[name]
	if !ok || time.Now().After(cached.deadline) {
		return origin.Entry{}, false
	}
	return cached.entry, true
}

func (n *node) cacheChildren(entries []origin.Entry, deadline time.Time) {
	n.mu.Lock()
	n.children = make(map[string]cachedEntry, len(entries))
	for _, entry := range entries {
		n.children[entry.Name] = cachedEntry{entry: entry, deadline: deadline}
	}
	n.mu.Unlock()
}

func inodeNumber(name string, isDir bool) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	if isDir {
		_, _ = h.Write([]byte{1})
	}
	ino := h.Sum64()
	if ino == 0 || ino == 1 {
		ino += 2
	}
	return ino
}

func modeFor(entry origin.Entry) uint32 {
	if entry.IsDir {
		return fuse.S_IFDIR | 0o555
	}
	return fuse.S_IFREG | 0o444
}

func fillAttr(entry origin.Entry, attr *fuse.Attr) {
	attr.Ino = inodeNumber(entry.Path, entry.IsDir)
	attr.Mode = modeFor(entry)
	attr.Nlink = 1
	if entry.IsDir {
		attr.Nlink = 2
	} else if entry.Size > 0 {
		attr.Size = uint64(entry.Size)
	}
	attr.Blksize = 4096
	if !entry.ModTime.IsZero() {
		mtime := entry.ModTime
		attr.SetTimes(&mtime, &mtime, &mtime)
	}
}

func (n *node) logError(operation string, err error) {
	// Negative lookups are normal filesystem answers. Applications routinely
	// probe names such as .git and HEAD; the kernel still receives ENOENT, while
	// debug-level go-fuse logging remains available when that detail is needed.
	if n.logger == nil || errors.Is(err, origin.ErrNotFound) {
		return
	}
	n.logger.Printf("%s path=%q error=%v", operation, n.path, err)
}

func errno(err error) syscall.Errno {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, origin.ErrNotFound):
		return syscall.ENOENT
	case errors.Is(err, origin.ErrNotDir):
		return syscall.ENOTDIR
	case errors.Is(err, origin.ErrIsDir):
		return syscall.EISDIR
	case errors.Is(err, context.Canceled):
		return syscall.EINTR
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		return syscall.EACCES
	default:
		return syscall.EIO
	}
}

func (n *node) Getattr(ctx context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	entry, ok := n.cachedStat()
	if !ok {
		var err error
		entry, err = n.service.Origin().Stat(ctx, n.path)
		if err != nil {
			n.logError("getattr", err)
			return errno(err)
		}
		n.cacheStat(entry, time.Now().Add(n.attrTTL))
	}
	fillAttr(entry, &out.Attr)
	out.SetTimeout(n.attrTTL)
	return 0
}

func (n *node) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	childPath := name
	if n.path != "." {
		childPath = path.Join(n.path, name)
	}
	entry, ok := n.cachedChild(name)
	if !ok {
		var err error
		entry, err = n.service.Origin().Stat(ctx, childPath)
		if err != nil {
			n.logError("lookup "+childPath, err)
			return nil, errno(err)
		}
	}
	deadline := time.Now().Add(n.attrTTL)
	fillAttr(entry, &out.Attr)
	out.SetEntryTimeout(n.attrTTL)
	out.SetAttrTimeout(n.attrTTL)
	childNode := &node{
		service: n.service, path: entry.Path, attrTTL: n.attrTTL, logger: n.logger,
		processes: n.processes, logLevel: n.logLevel,
	}
	childNode.cacheStat(entry, deadline)
	child := n.NewInode(ctx, childNode, fs.StableAttr{Mode: modeFor(entry) & syscall.S_IFMT, Ino: inodeNumber(entry.Path, entry.IsDir)})
	return child, 0
}

func (n *node) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	entries, err := n.service.Origin().ReadDir(ctx, n.path)
	if err != nil {
		n.logError("readdir", err)
		return nil, errno(err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	n.cacheChildren(entries, time.Now().Add(n.attrTTL))
	dirEntries := make([]fuse.DirEntry, 0, len(entries))
	for _, entry := range entries {
		dirEntries = append(dirEntries, fuse.DirEntry{
			Name: entry.Name, Ino: inodeNumber(entry.Path, entry.IsDir), Mode: modeFor(entry) & syscall.S_IFMT,
		})
	}
	return fs.NewListDirStream(dirEntries), 0
}

func (n *node) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if flags&syscall.O_ACCMODE != syscall.O_RDONLY {
		return nil, 0, syscall.EROFS
	}
	entry, ok := n.cachedStat()
	if !ok {
		var err error
		entry, err = n.service.Origin().Stat(ctx, n.path)
		if err != nil {
			n.logError("open-stat", err)
			return nil, 0, errno(err)
		}
		n.cacheStat(entry, time.Now().Add(n.attrTTL))
	}
	included := n.service.matches(entry.Path)
	candidate := n.service.StubCandidate(entry)
	var (
		matched  bool
		caller   callerInfo
		matchErr error
	)
	if included || n.logLevel.logAllAccesses() {
		matched, caller, matchErr = n.processes.match(ctx)
	}
	stubAllowed := candidate && matched && matchErr == nil
	view, stubbed, err := n.service.OpenView(ctx, entry, stubAllowed)
	if err != nil {
		n.logAccess(caller, included, matchErr, "error")
		n.logError("open-view", err)
		return nil, 0, errno(err)
	}
	route := "passthrough"
	if stubbed {
		route = "stub"
	}
	n.logAccess(caller, included, matchErr, route)
	openFlags := uint32(fuse.FOPEN_KEEP_CACHE)
	if stubbed {
		// The same inode returns different bytes to selected processes. Direct
		// I/O keeps stub pages out of the shared kernel page cache.
		openFlags = fuse.FOPEN_DIRECT_IO
	}
	return &fileHandle{view: view}, openFlags, 0
}

func (n *node) logAccess(caller callerInfo, included bool, processErr error, route string) {
	if n.logger == nil || (!included && !n.logLevel.logAllAccesses()) {
		return
	}
	if processErr != nil {
		n.logger.Printf("access pid=%d uid=%d gid=%d process=%q path=%q include=%t route=%s process_error=%q",
			caller.pid, caller.uid, caller.gid, caller.comm, n.path, included, route, processErr)
		return
	}
	n.logger.Printf("access pid=%d uid=%d gid=%d process=%q path=%q include=%t route=%s",
		caller.pid, caller.uid, caller.gid, caller.comm, n.path, included, route)
}

type fileHandle struct {
	view View
}

func (h *fileHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	if off < 0 {
		return nil, syscall.EINVAL
	}
	if off >= h.view.Size() || len(dest) == 0 {
		return fuse.ReadResultData(nil), 0
	}
	if remain := h.view.Size() - off; int64(len(dest)) > remain {
		dest = dest[:remain]
	}
	n, err := h.view.ReadAt(ctx, dest, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, errno(err)
	}
	return fuse.ReadResultData(dest[:n]), 0
}

func (h *fileHandle) Release(context.Context) syscall.Errno {
	return errno(h.view.Close())
}

var (
	_ fs.NodeGetattrer = (*node)(nil)
	_ fs.NodeLookuper  = (*node)(nil)
	_ fs.NodeReaddirer = (*node)(nil)
	_ fs.NodeOpener    = (*node)(nil)
	_ fs.FileReader    = (*fileHandle)(nil)
	_ fs.FileReleaser  = (*fileHandle)(nil)
)
