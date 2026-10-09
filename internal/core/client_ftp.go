package core

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"strconv"
	"sync"

	"github.com/jlaffaye/ftp"
)

// ftpClient wraps one FTP control connection; a mutex serialises commands because the
// protocol cannot interleave them.
type ftpClient struct {
	proto string
	mu    sync.Mutex
	c     *ftp.ServerConn
}

func dialFTP(ctx context.Context, p ConnectParams) (RemoteClient, error) {
	opts := []ftp.DialOption{
		ftp.DialWithContext(ctx),
		ftp.DialWithTimeout(p.timeout()),
	}
	if p.Protocol == ProtoFTPS {
		opts = append(opts, ftp.DialWithExplicitTLS(&tls.Config{
			ServerName:         p.Host,
			InsecureSkipVerify: p.SkipTLSVerify,
			MinVersion:         tls.VersionTLS12,
		}))
	}
	c, err := ftp.Dial(net.JoinHostPort(p.Host, strconv.Itoa(p.port())), opts...)
	if err != nil {
		return nil, err
	}
	user, pass := p.User, p.Password
	if user == "" {
		user, pass = "anonymous", "ftpapp@localhost"
	}
	if err := c.Login(user, pass); err != nil {
		c.Quit()
		return nil, err
	}
	return &ftpClient{proto: p.Protocol, c: c}, nil
}

func (f *ftpClient) Protocol() string { return f.proto }
func (f *ftpClient) Caps() Caps       { return Caps{Browse: true, Modify: true} }

func (f *ftpClient) Getwd() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.c.CurrentDir()
}

func (f *ftpClient) List(dir string) ([]RemoteEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entries, err := f.c.List(dir)
	if err != nil {
		return nil, err
	}
	out := make([]RemoteEntry, 0, len(entries))
	for _, e := range entries {
		if e.Name == "." || e.Name == ".." {
			continue
		}
		out = append(out, RemoteEntry{
			Name:    e.Name,
			Size:    int64(e.Size),
			IsDir:   e.Type == ftp.EntryTypeFolder,
			IsLink:  e.Type == ftp.EntryTypeLink,
			ModTime: e.Time,
		})
	}
	sortEntries(out)
	return out, nil
}

func (f *ftpClient) Mkdir(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.c.MakeDir(p)
}

func (f *ftpClient) Remove(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.c.Delete(p)
}

func (f *ftpClient) RemoveDir(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.c.RemoveDir(p)
}

func (f *ftpClient) Rename(from, to string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.c.Rename(from, to)
}

func (f *ftpClient) Get(ctx context.Context, remote string, w io.Writer, progress BytesFunc) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	total := int64(-1) // SIZE is optional; progress then has no total
	if n, err := f.c.FileSize(remote); err == nil {
		total = n
	}
	resp, err := f.c.Retr(remote)
	if err != nil {
		return err
	}
	t := newTracker(ctx, total, progress)
	_, err = io.Copy(&trackedWriter{w: w, t: t}, resp)
	// Close also reads the server's final reply, which is an error after an aborted transfer.
	if cerr := resp.Close(); err == nil && ctx.Err() == nil {
		err = cerr
	}
	t.finish()
	return ctxErr(ctx, err)
}

func (f *ftpClient) Put(ctx context.Context, remote string, r io.Reader, size int64, progress BytesFunc) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := newTracker(ctx, size, progress)
	err := f.c.Stor(remote, &trackedReader{r: r, t: t})
	t.finish()
	return ctxErr(ctx, err)
}

func (f *ftpClient) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.c.Quit()
}
