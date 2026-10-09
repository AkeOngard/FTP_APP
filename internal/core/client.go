package core

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

const (
	ProtoFTP  = "ftp"
	ProtoFTPS = "ftps" // explicit TLS (AUTH TLS)
	ProtoSFTP = "sftp"
	ProtoTFTP = "tftp"
)

const defaultConnectTimeout = 15 * time.Second

// ConnectParams describes a remote server. Passwords are never stored in the config.
type ConnectParams struct {
	Protocol      string `json:"protocol"`
	Host          string `json:"host"`
	Port          int    `json:"port"` // 0 = protocol default
	User          string `json:"user"`
	Password      string `json:"password"`
	KeyFile       string `json:"keyFile"` // SFTP: private key file, as an alternative to a password
	KeyPassphrase string `json:"keyPassphrase"`
	SkipTLSVerify bool   `json:"skipTlsVerify"` // FTPS: accept self-signed certificates
	TimeoutSecs   int    `json:"timeoutSecs"`   // 0 = default
	BlockSize     int    `json:"blockSize"`     // TFTP block size in bytes; 0 = default
	Parallel      int    `json:"parallel"`      // simultaneous transfers (connections); 0 = default
}

// Parallel transfers: several connections move files at once, which helps most with many small files and
// slow links. Servers often limit the connections per user, so the default is modest; the pool also backs
// off by itself when a server refuses an extra connection.
const (
	defaultParallel = 4
	maxParallel     = 16
)

func (p ConnectParams) parallel() int {
	switch {
	case p.Parallel <= 0:
		return defaultParallel
	case p.Parallel > maxParallel:
		return maxParallel
	}
	return p.Parallel
}

// TFTP block sizes: 512 is what every server understands, 1468 fills an Ethernet frame without IP
// fragmentation, and RFC 2348 allows up to 65464. Bigger blocks are much faster (throughput scales with
// the block size) but rely on the network passing fragmented datagrams, so the default stays at 1468.
const (
	tftpMinBlock     = 512
	tftpDefaultBlock = 1468
	tftpMaxBlock     = 65464
)

// tftpPlatformMaxBlock is the largest block this system can send in one datagram. macOS and the BSDs
// refuse UDP datagrams above 9216 bytes by default (net.inet.udp.maxdgram), so a bigger block would
// make every transfer fail there; Windows and Linux take the protocol's maximum.
func tftpPlatformMaxBlock() int {
	switch runtime.GOOS {
	case "darwin", "freebsd", "netbsd", "openbsd", "dragonfly":
		return 8192
	}
	return tftpMaxBlock
}

func (p ConnectParams) blockSize() int {
	switch max := tftpPlatformMaxBlock(); {
	case p.BlockSize == 0:
		return tftpDefaultBlock
	case p.BlockSize < tftpMinBlock:
		return tftpMinBlock
	case p.BlockSize > max:
		return max
	}
	return p.BlockSize
}

func (p ConnectParams) port() int {
	if p.Port != 0 {
		return p.Port
	}
	switch p.Protocol {
	case ProtoSFTP:
		return 22
	case ProtoTFTP:
		return 69
	default:
		return 21
	}
}

func (p ConnectParams) timeout() time.Duration {
	if p.TimeoutSecs > 0 {
		return time.Duration(p.TimeoutSecs) * time.Second
	}
	return defaultConnectTimeout
}

func (p ConnectParams) validate() error {
	switch p.Protocol {
	case ProtoFTP, ProtoFTPS, ProtoSFTP, ProtoTFTP:
	default:
		return fmt.Errorf("unknown protocol %q", p.Protocol)
	}
	if strings.TrimSpace(p.Host) == "" {
		return fmt.Errorf("host must not be empty")
	}
	if p.Port < 0 || p.Port > 65535 {
		return fmt.Errorf("port %d out of range", p.Port)
	}
	return nil
}

// RemoteEntry is one item of a remote directory listing.
type RemoteEntry struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	IsDir   bool      `json:"isDir"`
	IsLink  bool      `json:"isLink"`
	ModTime time.Time `json:"modTime"`
	Mode    string    `json:"mode"` // permission string where the protocol provides one (SFTP)
}

// Caps tells the UI what a protocol can do; TFTP can only transfer files.
type Caps struct {
	Browse bool `json:"browse"` // Getwd, List
	Modify bool `json:"modify"` // Mkdir, Remove, RemoveDir, Rename
}

// BytesFunc reports progress of one file; total is -1 when the size is unknown.
type BytesFunc func(done, total int64)

// ProgressFunc reports progress during multi-file operations.
type ProgressFunc func(file string, done, total int64)

// RemoteClient is the protocol-independent view of a remote server. Remote paths use "/".
// FTP clients serialise calls on one connection; open several connections for parallel transfers.
type RemoteClient interface {
	Protocol() string
	Caps() Caps
	Getwd() (string, error)
	List(dir string) ([]RemoteEntry, error)
	Mkdir(p string) error
	Remove(p string) error    // a file
	RemoveDir(p string) error // an empty directory
	Rename(from, to string) error
	// Get streams remote to w. Cancelling ctx aborts the transfer with ctx.Err().
	Get(ctx context.Context, remote string, w io.Writer, progress BytesFunc) error
	// Put streams r (size bytes, or -1 if unknown) to remote, replacing it.
	Put(ctx context.Context, remote string, r io.Reader, size int64, progress BytesFunc) error
	Close() error
}

// Connect opens a connection described by p. known is used to verify SFTP host keys.
func Connect(ctx context.Context, p ConnectParams, known *KnownHosts) (RemoteClient, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	switch p.Protocol {
	case ProtoFTP, ProtoFTPS:
		return dialFTP(ctx, p)
	case ProtoSFTP:
		return dialSFTP(ctx, p, known)
	default:
		return dialTFTP(p)
	}
}

// tracker turns raw byte counts into throttled progress callbacks and honours cancellation.
type tracker struct {
	ctx      context.Context
	total    int64
	fn       BytesFunc
	done     atomic.Int64
	lastTick atomic.Int64 // unix nanos of the last callback
}

func newTracker(ctx context.Context, total int64, fn BytesFunc) *tracker {
	return &tracker{ctx: ctx, total: total, fn: fn}
}

const progressInterval = 100 * time.Millisecond

func (t *tracker) add(n int) {
	done := t.done.Add(int64(n))
	if t.fn == nil {
		return
	}
	now := time.Now().UnixNano()
	if last := t.lastTick.Load(); now-last >= int64(progressInterval) && t.lastTick.CompareAndSwap(last, now) {
		t.fn(done, t.total)
	}
}

// finish emits the final count so the UI always ends on the true value.
func (t *tracker) finish() {
	if t.fn != nil {
		t.fn(t.done.Load(), t.total)
	}
}

type trackedReader struct {
	r io.Reader
	t *tracker
}

func (tr *trackedReader) Read(p []byte) (int, error) {
	if err := tr.t.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := tr.r.Read(p)
	tr.t.add(n)
	return n, err
}

type trackedWriter struct {
	w io.Writer
	t *tracker
}

func (tw *trackedWriter) Write(p []byte) (int, error) {
	if err := tw.t.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := tw.w.Write(p)
	tw.t.add(n)
	return n, err
}

// ctxErr prefers the context's error over the secondary error caused by aborting the transfer.
func ctxErr(ctx context.Context, err error) error {
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
