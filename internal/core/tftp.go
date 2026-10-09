package core

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pin/tftp/v3"
)

// maxTFTPBlockSize is the largest block size the server grants. Blocks above the network's MTU are
// fragmented by IP, which works on a clean LAN and is what makes big blocks fast there.
const maxTFTPBlockSize = 16384

// tftpPartSuffix marks an upload that is still coming in.
const tftpPartSuffix = ".tftp-part"

type tftpService struct {
	cfg TFTPConfig
	log *Logger
	mon *Monitor

	mu      sync.Mutex
	srv     *tftp.Server
	conn    net.PacketConn
	sandbox *Sandbox
}

func newTFTPService(cfg TFTPConfig, log *Logger, mon *Monitor) *tftpService {
	return &tftpService{cfg: cfg, log: log, mon: mon}
}

func (s *tftpService) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv != nil {
		return errors.New("already running")
	}
	sb, err := OpenSandbox(s.cfg.Root)
	if err != nil {
		return fmt.Errorf("root folder: %w", err)
	}
	conn, err := net.ListenPacket("udp", listenAddr(s.cfg.ServiceConfig))
	if err != nil {
		sb.Close()
		return err
	}
	view := sb.View(s.cfg.ReadOnly)

	var writeHandler func(string, io.WriterTo) error
	if !s.cfg.ReadOnly {
		writeHandler = func(name string, wt io.WriterTo) error { return s.receive(view, name, wt) }
	}
	srv := tftp.NewServer(
		func(name string, rf io.ReaderFrom) error { return s.send(view, name, rf) },
		writeHandler,
	)
	srv.SetTimeout(5 * time.Second)
	// By default the library clamps every transfer to 512-byte blocks, which caps a transfer at about
	// 30 MB/s however fast the network is. Honour the block size a client asks for (RFC 2348) up to a
	// cap; clients that ask for nothing still get 512, so old devices are unaffected.
	srv.SetBlockSizeNegotiation(false)
	srv.SetBlockSize(min(maxTFTPBlockSize, tftpPlatformMaxBlock()))
	srv.SetHook(tftpHook{s.log})

	s.srv, s.conn, s.sandbox = srv, conn, sb
	go func() {
		if err := srv.Serve(conn); err != nil && !errors.Is(err, net.ErrClosed) {
			s.log.Errorf(ServiceTFTP, "server stopped: %v", err)
		}
	}()
	mode := "read/write"
	if s.cfg.ReadOnly {
		mode = "read-only"
	}
	s.log.Infof(ServiceTFTP, "listening on %s (%s), root %s", conn.LocalAddr(), mode, s.cfg.Root)
	return nil
}

func (s *tftpService) Stop() error {
	s.mu.Lock()
	srv, sb := s.srv, s.sandbox
	s.srv, s.conn, s.sandbox = nil, nil, nil
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	srv.Shutdown()
	sb.Close()
	s.log.Infof(ServiceTFTP, "stopped")
	return nil
}

func (s *tftpService) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.srv != nil
}

func (s *tftpService) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		return ""
	}
	return s.conn.LocalAddr().String()
}

// tftpName normalises names from embedded devices that send Windows-style separators.
func tftpName(name string) string { return strings.ReplaceAll(name, "\\", "/") }

func (s *tftpService) send(fs *SandboxFS, name string, rf io.ReaderFrom) error {
	f, err := fs.Open(tftpName(name))
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.IsDir() {
		return fmt.Errorf("%s is a directory", name)
	}
	remote := ""
	if t, ok := rf.(tftp.OutgoingTransfer); ok {
		t.SetSize(st.Size())
		a := t.RemoteAddr()
		remote = a.String()
	}
	h := s.mon.Begin(ServiceTFTP, "-", remote, "download", name, st.Size())
	_, err = rf.ReadFrom(&meterReader{r: f, h: h})
	h.End(err)
	return err
}

func (s *tftpService) receive(fs *SandboxFS, name string, wt io.WriterTo) error {
	// TFTP runs over UDP with no login, and devices use it to store configs and firmware: an upload that
	// breaks off must not leave a truncated file under the real name, nor destroy the previous one. The
	// data goes to a side file that replaces the target only when the transfer completed.
	target := tftpName(name)
	if st, err := fs.Stat(target); err == nil && st.IsDir() {
		return fmt.Errorf("%s is a directory", name)
	}
	part := target + tftpPartSuffix
	f, err := fs.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	done := false
	defer func() {
		f.Close()
		if !done {
			fs.Remove(part)
		}
	}()
	remote := ""
	total := int64(-1)
	if in, ok := wt.(tftp.IncomingTransfer); ok {
		a := in.RemoteAddr()
		remote = a.String()
		if n, ok := in.Size(); ok { // the client's tsize option, when it sends one
			total = n
		}
	}
	h := s.mon.Begin(ServiceTFTP, "-", remote, "upload", name, total)
	_, err = wt.WriteTo(&meterWriter{w: f, h: h})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = fs.Rename(part, target)
	}
	done = err == nil
	h.End(err)
	return err
}

type meterReader struct {
	r io.Reader
	h *TransferHandle
}

func (m *meterReader) Read(p []byte) (int, error) {
	n, err := m.r.Read(p)
	m.h.Add(n)
	return n, err
}

type meterWriter struct {
	w io.Writer
	h *TransferHandle
}

func (m *meterWriter) Write(p []byte) (int, error) {
	n, err := m.w.Write(p)
	m.h.Add(n)
	return n, err
}

type tftpHook struct{ log *Logger }

// The monitor already logs every transfer with sizes and speed; the library hook is left empty.
func (h tftpHook) OnSuccess(tftp.TransferStats)        {}
func (h tftpHook) OnFailure(tftp.TransferStats, error) {}
