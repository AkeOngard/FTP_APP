package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type sftpClient struct {
	ssh *ssh.Client
	c   *sftp.Client
}

func dialSFTP(ctx context.Context, p ConnectParams, known *KnownHosts) (RemoteClient, error) {
	var auth []ssh.AuthMethod
	if p.KeyFile != "" {
		signer, err := loadSigner(p.KeyFile, p.KeyPassphrase)
		if err != nil {
			return nil, err
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}
	if p.Password != "" {
		auth = append(auth, ssh.Password(p.Password),
			// Some servers only offer keyboard-interactive for what is effectively a password login.
			ssh.KeyboardInteractive(func(_, _ string, qs []string, _ []bool) ([]string, error) {
				answers := make([]string, len(qs))
				for i := range answers {
					answers[i] = p.Password
				}
				return answers, nil
			}))
	}
	if len(auth) == 0 {
		return nil, errors.New("SFTP needs a password or a key file")
	}
	if known == nil {
		return nil, errors.New("host key store is required for SFTP")
	}

	addr := net.JoinHostPort(p.Host, strconv.Itoa(p.port()))
	cfg := &ssh.ClientConfig{
		User:              p.User,
		Auth:              auth,
		HostKeyCallback:   known.Callback(),
		HostKeyAlgorithms: known.algorithms(addr),
		Timeout:           p.timeout(),
	}
	d := net.Dialer{Timeout: p.timeout()}
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	// The handshake has no context support; bound it with a deadline and abort it on cancel.
	nc.SetDeadline(time.Now().Add(p.timeout()))
	stop := context.AfterFunc(ctx, func() { nc.Close() })
	sc, chans, reqs, err := ssh.NewClientConn(nc, addr, cfg)
	stop()
	if err != nil {
		nc.Close()
		return nil, ctxErr(ctx, err)
	}
	nc.SetDeadline(time.Time{})
	sshClient := ssh.NewClient(sc, chans, reqs)

	c, err := sftp.NewClient(sshClient)
	if err != nil {
		sshClient.Close()
		return nil, err
	}
	return &sftpClient{ssh: sshClient, c: c}, nil
}

func loadSigner(path, passphrase string) (ssh.Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("key file: %w", err)
	}
	var signer ssh.Signer
	if passphrase != "" {
		signer, err = ssh.ParsePrivateKeyWithPassphrase(data, []byte(passphrase))
	} else {
		signer, err = ssh.ParsePrivateKey(data)
	}
	var missing *ssh.PassphraseMissingError
	if errors.As(err, &missing) {
		return nil, errors.New("key file is encrypted; a passphrase is required")
	}
	if err != nil {
		return nil, fmt.Errorf("key file: %w", err)
	}
	return signer, nil
}

func (s *sftpClient) Protocol() string { return ProtoSFTP }
func (s *sftpClient) Caps() Caps       { return Caps{Browse: true, Modify: true} }

func (s *sftpClient) Getwd() (string, error) { return s.c.Getwd() }

func (s *sftpClient) List(dir string) ([]RemoteEntry, error) {
	infos, err := s.c.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]RemoteEntry, 0, len(infos))
	for _, fi := range infos {
		e := RemoteEntry{
			Name:    fi.Name(),
			Size:    fi.Size(),
			IsDir:   fi.IsDir(),
			ModTime: fi.ModTime(),
			Mode:    fi.Mode().String(),
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			e.IsLink = true
			// Report what the link points at so the UI can show it as a folder or a file.
			if target, err := s.c.Stat(joinRemote(dir, fi.Name())); err == nil {
				e.IsDir, e.Size = target.IsDir(), target.Size()
			}
		}
		out = append(out, e)
	}
	sortEntries(out)
	return out, nil
}

func (s *sftpClient) Mkdir(p string) error     { return s.c.Mkdir(p) }
func (s *sftpClient) Remove(p string) error    { return s.c.Remove(p) }
func (s *sftpClient) RemoveDir(p string) error { return s.c.RemoveDirectory(p) }

// Rename replaces an existing target where the server supports POSIX rename.
func (s *sftpClient) Rename(from, to string) error {
	if err := s.c.PosixRename(from, to); err == nil {
		return nil
	}
	return s.c.Rename(from, to)
}

func (s *sftpClient) Get(ctx context.Context, remote string, w io.Writer, progress BytesFunc) error {
	f, err := s.c.Open(remote)
	if err != nil {
		return err
	}
	defer f.Close()
	total := int64(-1)
	if st, err := f.Stat(); err == nil {
		total = st.Size()
	}
	stop := context.AfterFunc(ctx, func() { f.Close() }) // unblocks a stalled read
	defer stop()
	t := newTracker(ctx, total, progress)
	// WriteTo issues concurrent reads, which is much faster than a plain io.Copy.
	_, err = f.WriteTo(&trackedWriter{w: w, t: t})
	t.finish()
	return ctxErr(ctx, err)
}

func (s *sftpClient) Put(ctx context.Context, remote string, r io.Reader, size int64, progress BytesFunc) error {
	f, err := s.c.Create(remote)
	if err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { f.Close() })
	defer stop()
	t := newTracker(ctx, size, progress)
	// ReadFrom issues concurrent writes.
	_, err = f.ReadFrom(&trackedReader{r: r, t: t})
	if cerr := f.Close(); err == nil && ctx.Err() == nil {
		err = cerr
	}
	t.finish()
	return ctxErr(ctx, err)
}

// Close drops the SSH connection first: SFTP has no goodbye of its own, and sftp.Client.Close
// would otherwise wait for the server to close the channel, hanging on a misbehaving server.
func (s *sftpClient) Close() error {
	err := s.ssh.Close()
	s.c.Close() // only reports that the connection is already gone
	if errors.Is(err, net.ErrClosed) {
		err = nil
	}
	return err
}

func sortEntries(es []RemoteEntry) {
	sort.SliceStable(es, func(i, j int) bool {
		if es[i].IsDir != es[j].IsDir {
			return es[i].IsDir
		}
		return es[i].Name < es[j].Name
	})
}
