package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"ftpapp/internal/core"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const (
	appVersion  = "0.1.0"
	maxErrorLen = 600
)

// App is the object bound to the frontend: every exported method is callable from JavaScript.
type App struct {
	ctx context.Context
	mgr *core.Manager

	tmu       sync.Mutex
	transfers map[string]*transfer
	order     []string
	nextID    int
}

// transfer states.
const (
	stateQueued    = "queued"
	stateScanning  = "scanning" // measuring a folder so the progress can show a total
	stateRunning   = "running"
	stateDone      = "done"
	stateError     = "error"
	stateCancelled = "cancelled"
)

// TransferInfo is what the UI shows for each queued, running or finished transfer.
type TransferInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Direction string `json:"direction"` // "upload" | "download"
	State     string `json:"state"`
	File      string `json:"file"` // file currently being transferred (differs from Name for folders)
	Done      int64  `json:"done"` // bytes of the current file
	Total     int64  `json:"total"`

	// Whole-item figures: the same as the current file for a single file, the whole folder for a folder.
	OverallDone  int64   `json:"overallDone"`
	OverallTotal int64   `json:"overallTotal"` // -1 when unknown
	Files        int     `json:"files"`        // files completed
	ActiveFiles  int     `json:"activeFiles"`  // files being transferred right now (several at once for a folder)
	FilesTotal   int     `json:"filesTotal"`   // -1 when unknown
	Speed        float64 `json:"speed"`        // bytes per second, smoothed
	EtaSecs      int64   `json:"etaSecs"`      // -1 when unknown
	StartedAt    int64   `json:"startedAt"`    // unix milliseconds; 0 until it starts
	EndedAt      int64   `json:"endedAt"`      // unix milliseconds; 0 while active

	Error string `json:"error"`
}

// transferPlan tells queue what is known about a transfer before it starts.
type transferPlan struct {
	total int64 // bytes to move, or -1
	files int   // files to move, or -1
	// scan measures a folder on the transfer's own connection; nil when the size is already known.
	scan func(ctx context.Context, c core.RemoteClient) (files int, bytes int64, err error)
}

type transfer struct {
	info   TransferInfo
	cancel context.CancelFunc
}

func NewApp(mgr *core.Manager) *App {
	return &App{
		mgr:       mgr,
		transfers: map[string]*transfer{},
	}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.mgr.Log().Subscribe(func(e core.LogEntry) { a.emit("log", e) })
	a.mgr.Monitor().Subscribe(func(s core.TransferSnapshot) { a.emit("srvxfer", s) })
	a.mgr.StartAutoStart()
}

// busy reports whether any transfer is queued or running, on the client side or on our servers.
func (a *App) busy() bool {
	a.tmu.Lock()
	for _, t := range a.transfers {
		switch t.info.State {
		case stateQueued, stateScanning, stateRunning:
			a.tmu.Unlock()
			return true
		}
	}
	a.tmu.Unlock()
	return len(a.mgr.Monitor().Snapshot().Active) > 0
}

// TrimMemory hands unused memory back to the operating system. The UI calls it when the window has been
// idle or hidden for a while. It does nothing during a transfer, when the memory is about to be needed
// again anyway. Reports whether it ran.
func (a *App) TrimMemory() bool {
	if a.busy() {
		return false
	}
	debug.FreeOSMemory()
	trimWorkingSets()
	return true
}

// GetServerTransfers returns what our own servers are transferring right now, and the last few finished.
func (a *App) GetServerTransfers() core.TransferSnapshot { return a.mgr.Monitor().Snapshot() }

func (a *App) shutdown(context.Context) {
	a.tmu.Lock()
	for _, t := range a.transfers {
		t.cancel()
	}
	a.tmu.Unlock()
	a.mgr.Shutdown()
}

func (a *App) emit(event string, data any) {
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, event, data)
	}
}

// ---- general ----

type Info struct {
	Version   string `json:"version"`
	ConfigDir string `json:"configDir"`
	OS        string `json:"os"`
}

func (a *App) GetInfo() Info {
	return Info{Version: appVersion, ConfigDir: a.mgr.Dir(), OS: goos()}
}

// GetInitialPassword returns the generated admin password once after the first run, else "".
func (a *App) GetInitialPassword() string { return a.mgr.InitialPassword() }

// Slices returned to the frontend are never nil: a nil slice would arrive as JSON null.
func (a *App) GetLogs() []core.LogEntry { return a.mgr.Log().Entries() }

// ResolvePath returns the full path a configured shared folder points to: a relative path is
// relative to the folder the app runs from.
func (a *App) ResolvePath(root string) string { return a.mgr.ResolveRoot(root) }

// LocalIPs lists this machine's IPv4 addresses so the UI can show where servers can be reached.
func (a *App) LocalIPs() []string {
	ips := []string{}
	addrs, _ := net.InterfaceAddrs()
	for _, ad := range addrs {
		// Link-local (169.254.x.x) addresses belong to unconfigured adapters and are not useful to share.
		if ipn, ok := ad.(*net.IPNet); ok && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() && ipn.IP.To4() != nil {
			ips = append(ips, ipn.IP.String())
		}
	}
	return ips
}

// ---- dialogs ----

func (a *App) PickFolder(title string) (string, error) {
	return runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{Title: title})
}

func (a *App) PickFiles() ([]string, error) {
	return runtime.OpenMultipleFilesDialog(a.ctx, runtime.OpenDialogOptions{Title: "Choose files to upload"})
}

func (a *App) PickKeyFile() (string, error) {
	return runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{Title: "Choose a private key file"})
}

// ---- servers ----

func (a *App) GetStatus() []core.ServiceStatus { return a.mgr.Status() }

// GetSettings returns the config without password hashes; users are managed through SetUser/DeleteUser.
func (a *App) GetSettings() core.Config {
	cfg := a.mgr.Config()
	if cfg.Users == nil {
		cfg.Users = []core.User{}
	}
	for i := range cfg.Users {
		cfg.Users[i].PasswordHash = ""
	}
	return cfg
}

// SaveService stores the settings and restarts the named server if it is running, so the
// change takes effect. Users and sites in cfg are ignored: they have their own methods.
func (a *App) SaveService(name string, cfg core.Config) error {
	cur := a.mgr.Config()
	cfg.Users, cfg.Sites = cur.Users, cur.Sites
	if err := a.mgr.SetConfig(cfg); err != nil {
		return err
	}
	for _, st := range a.mgr.Status() {
		if st.Name == name && st.Running {
			return a.mgr.Restart(name)
		}
	}
	return nil
}

func (a *App) StartService(name string) error { return a.mgr.Start(name) }
func (a *App) StopService(name string) error  { return a.mgr.Stop(name) }

func (a *App) SetUser(name, password string, readOnly bool) error {
	return a.mgr.SetUser(name, password, readOnly)
}

func (a *App) DeleteUser(name string) error { return a.mgr.DeleteUser(name) }

// ---- saved sites and connections ----

func (a *App) GetSites() []core.Site {
	if s := a.mgr.Config().Sites; s != nil {
		return s
	}
	return []core.Site{}
}

func (a *App) SaveSite(s core.Site) error { return a.mgr.SaveSite(s) }

func (a *App) DeleteSite(name string) error { return a.mgr.DeleteSite(name) }

// TrustHost records the key the user just confirmed. The following Connect succeeds only if the server
// presents exactly this key, so a server that swaps keys between the question and the answer is refused.
func (a *App) TrustHost(host string, p HostPrompt) error {
	return a.mgr.KnownHosts().Trust(host, p.KeyType+" "+p.Fingerprint)
}

func (a *App) ForgetHost(host string, port int) error {
	return a.mgr.KnownHosts().Forget(net.JoinHostPort(host, strconv.Itoa(port)))
}

type ConnInfo struct {
	ID       string    `json:"id"`
	Protocol string    `json:"protocol"`
	Caps     core.Caps `json:"caps"`
	Cwd      string    `json:"cwd"`

	// UnknownHost is set instead of everything else when an SFTP server is seen for the first time:
	// nothing is connected yet. The UI shows the key, and after the user accepts it calls TrustHost
	// and Connect again.
	UnknownHost *HostPrompt `json:"unknownHost"`
}

// HostPrompt is what the user is asked to confirm before a new SFTP server is trusted.
type HostPrompt struct {
	Host        string `json:"host"`        // "host:port", passed back to TrustHost
	KeyType     string `json:"keyType"`     // e.g. "ssh-ed25519"
	Fingerprint string `json:"fingerprint"` // "SHA256:…", as ssh-keygen -l prints it
}

func (a *App) Connect(p core.ConnectParams) (ConnInfo, error) {
	id, err := a.mgr.Connect(context.Background(), p)
	var unknown *core.HostKeyUnknownError
	if errors.As(err, &unknown) {
		keyType, fp, _ := strings.Cut(unknown.Key, " ")
		return ConnInfo{UnknownHost: &HostPrompt{Host: unknown.Host, KeyType: keyType, Fingerprint: fp}}, nil
	}
	if err != nil {
		return ConnInfo{}, err
	}
	c, _ := a.mgr.Client(id)
	info := ConnInfo{ID: id, Protocol: c.Protocol(), Caps: c.Caps(), Cwd: "/"}
	if c.Caps().Browse {
		if wd, err := c.Getwd(); err == nil && wd != "" {
			info.Cwd = wd
		}
	}
	return info, nil
}

func (a *App) Disconnect(id string) error { return a.mgr.Disconnect(id) }

func (a *App) ListRemote(id, dir string) ([]core.RemoteEntry, error) {
	c, err := a.mgr.Client(id)
	if err != nil {
		return nil, err
	}
	return c.List(dir)
}

func (a *App) RemoteMkdir(id, p string) error {
	c, err := a.mgr.Client(id)
	if err != nil {
		return err
	}
	return c.Mkdir(p)
}

func (a *App) RemoteRename(id, from, to string) error {
	c, err := a.mgr.Client(id)
	if err != nil {
		return err
	}
	return c.Rename(from, to)
}

// RemoteDelete removes a file, or a directory with everything in it.
func (a *App) RemoteDelete(id, p string, isDir bool) error {
	c, err := a.mgr.Client(id)
	if err != nil {
		return err
	}
	if isDir {
		return core.RemoveTree(context.Background(), c, p)
	}
	return c.Remove(p)
}

// ---- local file browsing ----

type LocalEntry struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	IsDir   bool      `json:"isDir"`
	ModTime time.Time `json:"modTime"`
}

type LocalListing struct {
	Path    string       `json:"path"`
	Parent  string       `json:"parent"` // "" at a filesystem root
	Entries []LocalEntry `json:"entries"`
}

// ListLocal lists a local directory; an empty dir means the user's home folder.
func (a *App) ListLocal(dir string) (LocalListing, error) {
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return LocalListing{}, err
		}
		dir = home
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return LocalListing{}, err
	}
	des, err := os.ReadDir(abs)
	if err != nil {
		return LocalListing{}, err
	}
	out := LocalListing{Path: abs, Entries: make([]LocalEntry, 0, len(des))}
	if parent := filepath.Dir(abs); parent != abs {
		out.Parent = parent
	}
	for _, de := range des {
		info, err := de.Info()
		if err != nil {
			continue // vanished or unreadable; skip rather than fail the whole listing
		}
		isDir := de.IsDir()
		if de.Type()&os.ModeSymlink != 0 {
			if st, err := os.Stat(filepath.Join(abs, de.Name())); err == nil {
				isDir = st.IsDir()
			}
		}
		out.Entries = append(out.Entries, LocalEntry{Name: de.Name(), Size: info.Size(), IsDir: isDir, ModTime: info.ModTime()})
	}
	sort.SliceStable(out.Entries, func(i, j int) bool {
		x, y := out.Entries[i], out.Entries[j]
		if x.IsDir != y.IsDir {
			return x.IsDir
		}
		return strings.ToLower(x.Name) < strings.ToLower(y.Name)
	})
	return out, nil
}

// LocalRoots returns drive letters on Windows, "/" elsewhere, plus the home folder.
func (a *App) LocalRoots() []string {
	roots := []string{}
	if goos() == "windows" {
		for c := 'A'; c <= 'Z'; c++ {
			if _, err := os.Stat(string(c) + `:\`); err == nil {
				roots = append(roots, string(c)+`:\`)
			}
		}
	} else {
		roots = append(roots, "/")
	}
	return roots
}

func (a *App) LocalMkdir(parent, name string) error {
	if name == "" || name != filepath.Base(name) || name == "." || name == ".." {
		return errors.New("invalid folder name")
	}
	return os.Mkdir(filepath.Join(parent, name), 0o755)
}

// ---- transfers ----

type RemoteItem struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"isDir"`
	Size  int64  `json:"size"` // from the listing; used to show progress and time left
}

// Download queues one transfer per item into localDir.
func (a *App) Download(id string, items []RemoteItem, localDir string) error {
	if _, err := a.mgr.Client(id); err != nil {
		return err
	}
	if st, err := os.Stat(localDir); err != nil || !st.IsDir() {
		return fmt.Errorf("%s is not a folder", localDir)
	}
	for _, it := range items {
		it := it
		if it.Name == "" {
			it.Name = path.Base(it.Path)
		}
		dst := filepath.Join(localDir, it.Name)
		if !isSafeName(it.Name) {
			return fmt.Errorf("unsafe file name %q", it.Name)
		}
		plan := transferPlan{total: -1, files: -1}
		if it.IsDir {
			plan.scan = func(ctx context.Context, c core.RemoteClient) (int, int64, error) {
				return core.ScanRemote(ctx, c, it.Path)
			}
		} else {
			plan.files = 1
			if it.Size >= 0 {
				plan.total = it.Size
			}
		}
		var run runner
		if it.IsDir {
			run.tree = func(ctx context.Context, pool *core.ConnPool, opt core.TreeOptions) (int, error) {
				return core.DownloadTree(ctx, pool, it.Path, dst, opt)
			}
		} else {
			run.file = func(ctx context.Context, c core.RemoteClient, prog core.ProgressFunc) error {
				return core.DownloadFile(ctx, c, it.Path, dst, prog)
			}
		}
		a.queue(id, it.Name, "download", plan, run)
	}
	return nil
}

// Upload queues one transfer per local file or folder into remoteDir ("" for TFTP, which has no folders).
func (a *App) Upload(id string, localPaths []string, remoteDir string) error {
	if _, err := a.mgr.Client(id); err != nil {
		return err
	}
	for _, lp := range localPaths {
		st, err := os.Stat(lp)
		if err != nil {
			return err
		}
		name, rp, isDir := filepath.Base(lp), path.Join(remoteDir, filepath.Base(lp)), st.IsDir()
		lp := lp
		plan := transferPlan{total: -1, files: -1}
		if isDir {
			plan.scan = func(ctx context.Context, _ core.RemoteClient) (int, int64, error) {
				return core.ScanLocal(ctx, lp)
			}
		} else {
			plan.files, plan.total = 1, st.Size()
		}
		var run runner
		if isDir {
			run.tree = func(ctx context.Context, pool *core.ConnPool, opt core.TreeOptions) (int, error) {
				return core.UploadTree(ctx, pool, lp, rp, opt)
			}
		} else {
			run.file = func(ctx context.Context, c core.RemoteClient, prog core.ProgressFunc) error {
				return core.UploadFile(ctx, c, lp, rp, prog)
			}
		}
		a.queue(id, name, "upload", plan, run)
	}
	return nil
}

func isSafeName(n string) bool {
	return n != "" && n != "." && n != ".." && !strings.ContainsAny(n, `/\`+"\x00") && filepath.IsLocal(n)
}

// runner says how to move one queued item: a single file on one connection, or a folder on as many
// connections as the session's pool allows.
type runner struct {
	file func(ctx context.Context, c core.RemoteClient, prog core.ProgressFunc) error
	tree func(ctx context.Context, pool *core.ConnPool, opt core.TreeOptions) (files int, err error)
}

// queue registers a transfer. It runs on connections from the session's pool, never on the one used for
// browsing, so a long transfer cannot stall the file lists. Items wait their turn for a free connection,
// so the number of simultaneous transfers never exceeds the pool size, whether they come from one folder
// or from many selected items.
func (a *App) queue(sessionID, name, direction string, plan transferPlan, run runner) {
	ctx, cancel := context.WithCancel(context.Background())
	a.tmu.Lock()
	a.nextID++
	t := &transfer{
		info: TransferInfo{
			ID: "t" + strconv.Itoa(a.nextID), Name: name, Direction: direction, State: stateQueued,
			Total: -1, OverallTotal: plan.total, FilesTotal: plan.files, EtaSecs: -1,
		},
		cancel: cancel,
	}
	a.transfers[t.info.ID] = t
	a.order = append(a.order, t.info.ID)
	a.tmu.Unlock()
	a.publish(t)

	go func() {
		defer cancel()
		pool, err := a.mgr.Pool(sessionID)
		if err != nil {
			a.finish(t, 0, err)
			return
		}

		if run.tree == nil {
			// One file: wait (still "queued") until a connection is free, then move it.
			c, err := pool.Get(ctx)
			if err != nil {
				a.finish(t, 0, err)
				return
			}
			a.beginRunning(t)
			overall := a.newOverall(t)
			prog, _ := a.reporters(t, overall)
			err = run.file(ctx, c, prog)
			pool.Put(c, err != nil) // after an error the connection may be unusable: the pool replaces it
			files := 0
			if err == nil {
				files = 1
			}
			a.finish(t, files, err)
			return
		}

		// A folder: measure it first so the progress can show a total, then move its files in parallel.
		if plan.scan != nil {
			c, err := pool.Get(ctx)
			if err != nil {
				a.finish(t, 0, err)
				return
			}
			a.update(t, func(i *TransferInfo) { i.State = stateScanning })
			files, bytes, err := plan.scan(ctx, c)
			pool.Put(c, err != nil && ctx.Err() == nil)
			if err != nil { // cancelled while scanning
				a.finish(t, 0, err)
				return
			}
			a.update(t, func(i *TransferInfo) { i.FilesTotal, i.OverallTotal = files, bytes })
		}
		a.beginRunning(t) // time left is measured from when the bytes start to move
		overall := a.newOverall(t)
		prog, fileDone := a.reporters(t, overall)
		files, err := run.tree(ctx, pool, core.TreeOptions{Progress: prog, FileDone: fileDone})
		a.finish(t, files, err)
	}()
}

func (a *App) beginRunning(t *transfer) {
	a.update(t, func(i *TransferInfo) { i.State, i.StartedAt = stateRunning, nowMillis() })
}

func (a *App) newOverall(t *transfer) *core.Overall {
	a.tmu.Lock()
	defer a.tmu.Unlock()
	return core.NewOverall(t.info.OverallTotal, time.Now())
}

// reporters returns the callbacks the transfer code calls as bytes move and files complete. They can be
// called from several goroutines at once; a.update serialises them, which is also what protects overall.
func (a *App) reporters(t *transfer, overall *core.Overall) (core.ProgressFunc, func(string)) {
	apply := func(i *TransferInfo, st core.OverallState) {
		i.OverallDone, i.Files, i.ActiveFiles, i.Speed, i.EtaSecs = st.Done, st.FilesDone, st.Active, st.Speed, st.EtaSecs
	}
	progress := func(file string, done, total int64) {
		now := time.Now()
		a.update(t, func(i *TransferInfo) {
			st := overall.Update(file, done, now)
			i.File, i.Done, i.Total = file, done, total
			apply(i, st)
			if i.OverallTotal < 0 && i.FilesTotal == 1 && total >= 0 {
				i.OverallTotal = total // single file whose size was not known up front
				overall.Total = total
			}
		})
	}
	fileDone := func(file string) {
		now := time.Now()
		a.update(t, func(i *TransferInfo) { apply(i, overall.FileDone(file, now)) })
	}
	return progress, fileDone
}

func nowMillis() int64 { return time.Now().UnixMilli() }

func (a *App) finish(t *transfer, files int, err error) {
	a.update(t, func(i *TransferInfo) {
		i.Files = files
		i.ActiveFiles = 0
		i.EndedAt = nowMillis()
		i.EtaSecs = -1
		switch {
		case err == nil:
			i.State = stateDone
			if i.Total >= 0 {
				i.Done = i.Total
			}
			if i.OverallTotal >= 0 {
				i.OverallDone = i.OverallTotal
			} else {
				i.OverallTotal = i.OverallDone
			}
			if i.StartedAt > 0 && i.EndedAt > i.StartedAt {
				i.Speed = float64(i.OverallDone) / (float64(i.EndedAt-i.StartedAt) / 1000) // average over the whole transfer
			}
		case errors.Is(err, context.Canceled):
			i.State = stateCancelled
		default:
			i.State = stateError
			i.Error = err.Error()
			if len(i.Error) > maxErrorLen {
				i.Error = i.Error[:maxErrorLen] + "…"
			}
		}
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		a.mgr.Log().Errorf("transfer", "%s: %v", t.info.Name, err)
	}
}

func (a *App) update(t *transfer, mutate func(*TransferInfo)) {
	a.tmu.Lock()
	mutate(&t.info)
	a.tmu.Unlock()
	a.publish(t)
}

func (a *App) publish(t *transfer) {
	a.tmu.Lock()
	info := t.info
	a.tmu.Unlock()
	a.emit("transfer", info)
}

func (a *App) GetTransfers() []TransferInfo {
	a.tmu.Lock()
	defer a.tmu.Unlock()
	out := make([]TransferInfo, 0, len(a.order))
	for _, id := range a.order {
		out = append(out, a.transfers[id].info)
	}
	return out
}

func (a *App) CancelTransfer(id string) {
	a.tmu.Lock()
	t, ok := a.transfers[id]
	a.tmu.Unlock()
	if ok {
		t.cancel()
	}
}

// ClearFinished forgets transfers that are no longer active.
func (a *App) ClearFinished() []TransferInfo {
	a.tmu.Lock()
	keep := a.order[:0:0]
	for _, id := range a.order {
		switch a.transfers[id].info.State {
		case stateQueued, stateScanning, stateRunning:
			keep = append(keep, id)
		default:
			delete(a.transfers, id)
		}
	}
	a.order = keep
	a.tmu.Unlock()
	return a.GetTransfers()
}
