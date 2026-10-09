package core

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	ftpserver "github.com/fclairamb/ftpserverlib"
	"github.com/spf13/afero"
)

// Service is a start/stop-able network server.
type Service interface {
	Start() error
	Stop() error
	Running() bool
	Addr() string // actual listen address while running
}

const (
	ServiceFTP  = "ftp"
	ServiceSFTP = "sftp"
	ServiceTFTP = "tftp"
)

func listenAddr(c ServiceConfig) string {
	host := c.BindAddr
	if host == "" {
		host = "0.0.0.0"
	}
	return net.JoinHostPort(host, strconv.Itoa(c.Port))
}

type ftpService struct {
	cfg   FTPConfig
	users func() userDB // current users, looked up at each login
	log   *Logger
	mon   *Monitor
	dir   string // where the FTPS certificate lives
	guard *loginGuard

	tlsConf *tls.Config // set while running with cfg.TLS

	mu      sync.Mutex
	srv     *ftpserver.FtpServer
	drv     *ftpDriver
	sandbox *Sandbox
}

func newFTPService(cfg FTPConfig, users func() userDB, log *Logger, mon *Monitor, dir string) *ftpService {
	return &ftpService{cfg: cfg, users: users, log: log, mon: mon, dir: dir}
}

func (s *ftpService) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv != nil {
		return errors.New("already running")
	}
	if s.cfg.TLS {
		tc, err := loadOrCreateTLSConfig(s.dir)
		if err != nil {
			return fmt.Errorf("TLS certificate: %w", err)
		}
		s.tlsConf = tc
	}
	sb, err := OpenSandbox(s.cfg.Root)
	if err != nil {
		return fmt.Errorf("root folder: %w", err)
	}
	drv := &ftpDriver{svc: s, sandbox: sb, clients: map[uint32]ftpserver.ClientContext{}, release: map[uint32]func(){}}
	srv := ftpserver.NewFtpServer(drv)
	srv.Logger = slog.New(slog.DiscardHandler) // activity is reported through our own Logger
	if err := srv.Listen(); err != nil {
		sb.Close()
		return err
	}
	s.srv, s.drv, s.sandbox = srv, drv, sb

	go func() {
		if err := srv.Serve(); err != nil {
			s.log.Errorf(ServiceFTP, "server stopped: %v", err)
		}
	}()
	s.log.Infof(ServiceFTP, "listening on %s, root %s", srv.Addr(), s.cfg.Root)
	return nil
}

func (s *ftpService) Stop() error {
	s.mu.Lock()
	srv, drv, sb := s.srv, s.drv, s.sandbox
	s.srv, s.drv, s.sandbox = nil, nil, nil
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	err := srv.Stop()
	drv.closeClients()
	sb.Close()
	s.log.Infof(ServiceFTP, "stopped")
	return err
}

func (s *ftpService) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.srv != nil
}

func (s *ftpService) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv == nil {
		return ""
	}
	return s.srv.Addr()
}

// ftpDriver adapts our users and sandbox to ftpserverlib.
type ftpDriver struct {
	svc     *ftpService
	sandbox *Sandbox

	mu      sync.Mutex
	clients map[uint32]ftpserver.ClientContext
	release map[uint32]func() // gives the connection's slot back to the login guard
}

func (d *ftpDriver) closeClients() {
	d.mu.Lock()
	cs := make([]ftpserver.ClientContext, 0, len(d.clients))
	for _, c := range d.clients {
		cs = append(cs, c)
	}
	d.mu.Unlock()
	for _, c := range cs {
		c.Close()
	}
}

func (d *ftpDriver) GetSettings() (*ftpserver.Settings, error) {
	cfg := d.svc.cfg
	st := &ftpserver.Settings{
		ListenAddr:        listenAddr(cfg.ServiceConfig),
		PublicHost:        cfg.PublicHost,
		Banner:            "FTP_App FTP server ready",
		IdleTimeout:       300,
		ConnectionTimeout: 30,
		DisableSite:       true,
	}
	if cfg.PassiveStart > 0 {
		st.PassiveTransferPortRange = ftpserver.PortRange{Start: cfg.PassiveStart, End: cfg.PassiveEnd}
	}
	if cfg.RequireTLS {
		st.TLSRequired = ftpserver.MandatoryEncryption
	}
	return st, nil
}

func (d *ftpDriver) ClientConnected(cc ftpserver.ClientContext) (string, error) {
	// Refused connections are not logged: a flood of them would push everything else out of the log.
	release, err := d.svc.guard.Connect(cc.RemoteAddr())
	if err != nil {
		return err.Error(), err
	}
	d.mu.Lock()
	d.clients[cc.ID()] = cc
	d.release[cc.ID()] = release
	d.mu.Unlock()
	d.svc.log.Infof(ServiceFTP, "%s connected", cc.RemoteAddr())
	return "FTP_App FTP server ready", nil
}

func (d *ftpDriver) ClientDisconnected(cc ftpserver.ClientContext) {
	d.mu.Lock()
	release, admitted := d.release[cc.ID()]
	delete(d.clients, cc.ID())
	delete(d.release, cc.ID())
	d.mu.Unlock()
	if !admitted {
		return // it was refused on arrival
	}
	release()
	d.svc.log.Infof(ServiceFTP, "%s disconnected", cc.RemoteAddr())
}

func (d *ftpDriver) AuthUser(cc ftpserver.ClientContext, user, pass string) (ftpserver.ClientDriver, error) {
	cfg := d.svc.cfg
	readOnly := cfg.ReadOnly
	label := user

	if lu := strings.ToLower(user); cfg.AllowAnonymous && (lu == "anonymous" || lu == "ftp") {
		readOnly, label = true, "anonymous"
	} else if d.svc.guard.Blocked(cc.RemoteAddr()) {
		return nil, errLockedOut
	} else if u, ok := d.svc.users().Authenticate(user, pass); ok {
		readOnly = readOnly || u.ReadOnly
	} else {
		d.svc.log.Errorf(ServiceFTP, "%s: login failed for %q", cc.RemoteAddr(), user)
		d.svc.guard.Failed(cc.RemoteAddr())
		time.Sleep(500 * time.Millisecond) // slow down password guessing
		return nil, errors.New("invalid credentials")
	}
	d.svc.log.Infof(ServiceFTP, "%s: %q logged in", cc.RemoteAddr(), label)
	return &ftpFS{
		fs:     d.sandbox.View(readOnly),
		audit:  func(op, name string) { d.svc.log.Infof(ServiceFTP, "%s %s: %s", label, op, name) },
		mon:    d.svc.mon,
		user:   label,
		remote: cc.RemoteAddr().String(),
		alloc:  -1,
	}, nil
}

// GetTLSConfig enables AUTH TLS only when the config asks for it.
func (d *ftpDriver) GetTLSConfig() (*tls.Config, error) {
	if d.svc.tlsConf == nil {
		return nil, errors.New("TLS not enabled")
	}
	return d.svc.tlsConf, nil
}

// ftpFS adapts SandboxFS to afero.Fs for ftpserverlib and reports file transfers to the Monitor.
type ftpFS struct {
	fs     *SandboxFS
	audit  func(op, name string)
	mon    *Monitor
	user   string
	remote string

	allocMu sync.Mutex
	alloc   int64 // size announced by ALLO for the next upload; -1 when none
}

var (
	_ afero.Fs                                = (*ftpFS)(nil)
	_ ftpserver.ClientDriverExtensionAllocate = (*ftpFS)(nil)
)

// AllocateSpace handles ALLO, which some clients send before an upload to announce its size.
func (f *ftpFS) AllocateSpace(size int) error {
	f.allocMu.Lock()
	f.alloc = int64(size)
	f.allocMu.Unlock()
	return nil
}

func (f *ftpFS) takeAlloc() int64 {
	f.allocMu.Lock()
	defer f.allocMu.Unlock()
	n := f.alloc
	f.alloc = -1
	return n
}

func (f *ftpFS) Name() string { return "sandbox" }

func (f *ftpFS) Create(name string) (afero.File, error) {
	return f.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
}

func (f *ftpFS) Open(name string) (afero.File, error) {
	file, err := f.fs.Open(name)
	if err != nil {
		return nil, err
	}
	return f.trackRead(file, name), nil
}

func (f *ftpFS) OpenFile(name string, flag int, perm os.FileMode) (afero.File, error) {
	file, err := f.fs.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_APPEND|os.O_CREATE) != 0 {
		return f.track(file, "upload", name, f.takeAlloc()), nil
	}
	return f.trackRead(file, name), nil
}

// trackRead reports regular files as downloads; directories (listings) are left alone.
func (f *ftpFS) trackRead(file *os.File, name string) afero.File {
	st, err := file.Stat()
	if err != nil || st.IsDir() {
		return file
	}
	return f.track(file, "download", name, st.Size())
}

func (f *ftpFS) track(file afero.File, dir, name string, total int64) afero.File {
	return &trackedFile{File: file, upload: dir == "upload", h: f.mon.Begin(ServiceFTP, f.user, f.remote, dir, name, total)}
}

// trackedFile counts the bytes ftpserverlib moves through a file and reports how the transfer ended.
// It embeds the afero.File interface, not *os.File, so io.Copy cannot bypass Read/Write through the
// file's ReadFrom/WriteTo fast paths.
type trackedFile struct {
	afero.File
	upload bool
	h      *TransferHandle

	mu  sync.Mutex
	err error
}

func (t *trackedFile) Read(p []byte) (int, error) {
	n, err := t.File.Read(p)
	if !t.upload {
		t.h.Add(n)
	}
	return n, err
}

func (t *trackedFile) Write(p []byte) (int, error) {
	n, err := t.File.Write(p)
	if t.upload {
		t.h.Add(n)
	}
	return n, err
}

// TransferError is called by ftpserverlib when a transfer is aborted or the connection drops.
func (t *trackedFile) TransferError(err error) {
	t.mu.Lock()
	if t.err == nil {
		t.err = err
	}
	t.mu.Unlock()
}

func (t *trackedFile) Close() error {
	err := t.File.Close()
	t.mu.Lock()
	failed := t.err
	t.mu.Unlock()
	if failed == nil {
		failed = err
	}
	t.h.End(failed)
	return err
}

func (f *ftpFS) Mkdir(name string, perm os.FileMode) error {
	f.audit("mkdir", name)
	return f.fs.Mkdir(name, perm)
}

func (f *ftpFS) MkdirAll(name string, perm os.FileMode) error { return f.fs.MkdirAll(name, perm) }

func (f *ftpFS) Remove(name string) error {
	f.audit("delete", name)
	return f.fs.Remove(name)
}

func (f *ftpFS) RemoveAll(name string) error {
	f.audit("delete", name)
	return f.fs.RemoveAll(name)
}

func (f *ftpFS) Rename(oldName, newName string) error {
	f.audit("rename", oldName+" -> "+newName)
	return f.fs.Rename(oldName, newName)
}

func (f *ftpFS) Stat(name string) (os.FileInfo, error) { return f.fs.Stat(name) }

// Permission bits and ownership are not managed over FTP; accept and ignore them.
func (f *ftpFS) Chmod(string, os.FileMode) error { return nil }
func (f *ftpFS) Chown(string, int, int) error    { return nil }

func (f *ftpFS) Chtimes(name string, atime, mtime time.Time) error {
	return f.fs.Chtimes(name, atime, mtime)
}
