package core

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

const hostKeyFileName = "hostkey_ed25519"

type sftpService struct {
	cfg   SFTPConfig
	users func() userDB // current users, looked up at each login
	log   *Logger
	mon   *Monitor
	dir   string // where the host key lives
	guard *loginGuard

	mu      sync.Mutex
	ln      net.Listener
	sandbox *Sandbox
	conns   map[net.Conn]struct{}
}

func newSFTPService(cfg SFTPConfig, users func() userDB, log *Logger, mon *Monitor, dir string) *sftpService {
	return &sftpService{cfg: cfg, users: users, log: log, mon: mon, dir: dir}
}

func (s *sftpService) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln != nil {
		return errors.New("already running")
	}
	hostKey, err := loadOrCreateHostKey(filepath.Join(s.dir, hostKeyFileName))
	if err != nil {
		return fmt.Errorf("host key: %w", err)
	}
	sb, err := OpenSandbox(s.cfg.Root)
	if err != nil {
		return fmt.Errorf("root folder: %w", err)
	}
	ln, err := net.Listen("tcp", listenAddr(s.cfg.ServiceConfig))
	if err != nil {
		sb.Close()
		return err
	}
	sshCfg := &ssh.ServerConfig{
		ServerVersion: "SSH-2.0-FTPApp",
		MaxAuthTries:  3,
		PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if s.guard.Blocked(c.RemoteAddr()) {
				return nil, errLockedOut
			}
			u, ok := s.users().Authenticate(c.User(), string(pw))
			if !ok {
				s.log.Errorf(ServiceSFTP, "%s: login failed for %q", c.RemoteAddr(), c.User())
				s.guard.Failed(c.RemoteAddr())
				time.Sleep(500 * time.Millisecond) // slow down password guessing
				return nil, errors.New("invalid credentials")
			}
			ro := "0"
			if s.cfg.ReadOnly || u.ReadOnly {
				ro = "1"
			}
			return &ssh.Permissions{Extensions: map[string]string{"readonly": ro}}, nil
		},
	}
	sshCfg.AddHostKey(hostKey)

	s.ln, s.sandbox, s.conns = ln, sb, map[net.Conn]struct{}{}
	go s.acceptLoop(ln, sb, sshCfg)
	s.log.Infof(ServiceSFTP, "listening on %s, root %s", ln.Addr(), s.cfg.Root)
	return nil
}

func (s *sftpService) Stop() error {
	s.mu.Lock()
	ln, sb, conns := s.ln, s.sandbox, s.conns
	s.ln, s.sandbox, s.conns = nil, nil, nil
	open := make([]net.Conn, 0, len(conns))
	for c := range conns {
		open = append(open, c)
	}
	s.mu.Unlock()
	if ln == nil {
		return nil
	}
	err := ln.Close()
	for _, c := range open {
		c.Close()
	}
	sb.Close()
	s.log.Infof(ServiceSFTP, "stopped")
	return err
}

func (s *sftpService) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ln != nil
}

func (s *sftpService) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

func (s *sftpService) track(c net.Conn, add bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conns == nil {
		return
	}
	if add {
		s.conns[c] = struct{}{}
	} else {
		delete(s.conns, c)
	}
}

func (s *sftpService) acceptLoop(ln net.Listener, sb *Sandbox, sshCfg *ssh.ServerConfig) {
	for {
		nc, err := ln.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				s.log.Errorf(ServiceSFTP, "accept: %v", err)
			}
			return
		}
		// Blocked addresses and connections over the limit are dropped before the SSH handshake.
		release, err := s.guard.Connect(nc.RemoteAddr())
		if err != nil {
			nc.Close()
			continue
		}
		s.track(nc, true)
		go func() {
			defer release()
			defer s.track(nc, false)
			defer nc.Close()
			s.handleConn(nc, sb, sshCfg)
		}()
	}
}

func (s *sftpService) handleConn(nc net.Conn, sb *Sandbox, sshCfg *ssh.ServerConfig) {
	// Bound the handshake so idle sockets cannot pile up.
	nc.SetDeadline(time.Now().Add(30 * time.Second))
	sc, chans, reqs, err := ssh.NewServerConn(nc, sshCfg)
	if err != nil {
		return
	}
	nc.SetDeadline(time.Time{})
	defer sc.Close()

	user := sc.User()
	readOnly := sc.Permissions.Extensions["readonly"] == "1"
	s.log.Infof(ServiceSFTP, "%s: %q logged in", sc.RemoteAddr(), user)
	defer s.log.Infof(ServiceSFTP, "%s: %q disconnected", sc.RemoteAddr(), user)

	go ssh.DiscardRequests(reqs)
	audit := func(op, name string) { s.log.Infof(ServiceSFTP, "%s %s: %s", user, op, name) }

	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			newCh.Reject(ssh.UnknownChannelType, "only session channels are supported")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			return
		}
		go func() {
			defer ch.Close()
			for req := range chReqs {
				var sub struct{ Name string }
				ok := req.Type == "subsystem" && ssh.Unmarshal(req.Payload, &sub) == nil && sub.Name == "sftp"
				req.Reply(ok, nil)
				if !ok {
					continue
				}
				h := &sftpHandler{fs: sb.View(readOnly), audit: audit, mon: s.mon, user: user, remote: sc.RemoteAddr().String()}
				rs := sftp.NewRequestServer(ch, sftp.Handlers{FileGet: h, FilePut: h, FileCmd: h, FileList: h})
				if err := rs.Serve(); err != nil && !errors.Is(err, io.EOF) {
					s.log.Errorf(ServiceSFTP, "%s: session: %v", user, err)
				}
				rs.Close()
				return
			}
		}()
	}
}

func loadOrCreateHostKey(path string) (ssh.Signer, error) {
	if data, err := os.ReadFile(path); err == nil {
		return ssh.ParsePrivateKey(data)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(priv, "ftpapp host key")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(priv)
}

// sftpHandler serves pkg/sftp requests from a SandboxFS.
type sftpHandler struct {
	fs     *SandboxFS
	audit  func(op, name string)
	mon    *Monitor
	user   string
	remote string
}

// sftpFile is the file handed to pkg/sftp: it counts the bytes moved and reports how the transfer
// ended (pkg/sftp calls TransferError on failure and Close when the client is done).
type sftpFile struct {
	f *os.File
	h *TransferHandle

	mu  sync.Mutex
	err error
}

func (s *sftpFile) ReadAt(p []byte, off int64) (int, error) {
	n, err := s.f.ReadAt(p, off)
	s.h.Add(n)
	return n, err
}

func (s *sftpFile) WriteAt(p []byte, off int64) (int, error) {
	n, err := s.f.WriteAt(p, off)
	s.h.Add(n)
	return n, err
}

func (s *sftpFile) TransferError(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
}

func (s *sftpFile) Close() error {
	err := s.f.Close()
	s.mu.Lock()
	failed := s.err
	s.mu.Unlock()
	if failed == nil {
		failed = err
	}
	s.h.End(failed)
	return err
}

func (h *sftpHandler) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	f, err := h.fs.Open(r.Filepath)
	if err != nil {
		return nil, err
	}
	total := int64(-1)
	if st, err := f.Stat(); err == nil {
		total = st.Size()
	}
	return &sftpFile{f: f, h: h.mon.Begin(ServiceSFTP, h.user, h.remote, "download", r.Filepath, total)}, nil
}

func (h *sftpHandler) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	pf := r.Pflags()
	flag := os.O_WRONLY
	if pf.Creat {
		flag |= os.O_CREATE
	}
	if pf.Trunc {
		flag |= os.O_TRUNC
	}
	if pf.Excl {
		flag |= os.O_EXCL
	}
	if pf.Append {
		flag |= os.O_APPEND
	}
	f, err := h.fs.OpenFile(r.Filepath, flag, 0o644)
	if err != nil {
		return nil, err
	}
	// SFTP clients do not announce the size of an upload, so the total stays unknown.
	return &sftpFile{f: f, h: h.mon.Begin(ServiceSFTP, h.user, h.remote, "upload", r.Filepath, -1)}, nil
}

func (h *sftpHandler) Filecmd(r *sftp.Request) error {
	switch r.Method {
	case "Setstat":
		// Only modification times are applied; clients set them after uploads to preserve them.
		if r.AttrFlags().Acmodtime {
			a := r.Attributes()
			return h.fs.Chtimes(r.Filepath, time.Unix(int64(a.Atime), 0), time.Unix(int64(a.Mtime), 0))
		}
		return nil
	case "Rename":
		h.audit("rename", r.Filepath+" -> "+r.Target)
		return h.fs.Rename(r.Filepath, r.Target)
	case "Mkdir":
		h.audit("mkdir", r.Filepath)
		return h.fs.Mkdir(r.Filepath, 0o755)
	case "Rmdir":
		h.audit("delete", r.Filepath)
		return h.fs.Remove(r.Filepath)
	case "Remove":
		if st, err := h.fs.Lstat(r.Filepath); err == nil && st.IsDir() {
			return sftp.ErrSSHFxFailure // Remove is for files; directories go through Rmdir
		}
		h.audit("delete", r.Filepath)
		return h.fs.Remove(r.Filepath)
	default: // Link, Symlink
		return ErrUnsupported
	}
}

func (h *sftpHandler) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	switch r.Method {
	case "List":
		infos, err := h.fs.ReadDir(r.Filepath)
		if err != nil {
			return nil, err
		}
		return fileInfos(infos), nil
	case "Stat":
		st, err := h.fs.Stat(r.Filepath)
		if err != nil {
			return nil, err
		}
		return fileInfos{st}, nil
	case "Lstat":
		st, err := h.fs.Lstat(r.Filepath)
		if err != nil {
			return nil, err
		}
		return fileInfos{st}, nil
	default: // Readlink
		return nil, ErrUnsupported
	}
}

type fileInfos []os.FileInfo

func (l fileInfos) ListAt(dst []os.FileInfo, offset int64) (int, error) {
	if offset >= int64(len(l)) {
		return 0, io.EOF
	}
	n := copy(dst, l[offset:])
	if n < len(dst) {
		return n, io.EOF
	}
	return n, nil
}
