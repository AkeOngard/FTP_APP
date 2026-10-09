package core

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"

	"github.com/pin/tftp/v3"
)

// tftpClient only transfers files: the protocol has no listing, directories or deletion.
type tftpClient struct {
	c *tftp.Client
}

func dialTFTP(p ConnectParams) (RemoteClient, error) {
	c, err := tftp.NewClient(net.JoinHostPort(p.Host, strconv.Itoa(p.port())))
	if err != nil {
		return nil, err
	}
	c.SetTimeout(p.timeout())
	c.SetBlockSize(p.blockSize()) // a server that does not know the option answers with 512-byte blocks
	c.RequestTSize(true)          // lets downloads report a total size when the server supports it
	return &tftpClient{c: c}, nil
}

func (t *tftpClient) Protocol() string { return ProtoTFTP }
func (t *tftpClient) Caps() Caps       { return Caps{} }

func (t *tftpClient) Getwd() (string, error)             { return "/", nil }
func (t *tftpClient) List(string) ([]RemoteEntry, error) { return nil, ErrUnsupported }
func (t *tftpClient) Mkdir(string) error                 { return ErrUnsupported }
func (t *tftpClient) Remove(string) error                { return ErrUnsupported }
func (t *tftpClient) RemoveDir(string) error             { return ErrUnsupported }
func (t *tftpClient) Rename(string, string) error        { return ErrUnsupported }
func (t *tftpClient) Close() error                       { return nil }

// cleanTFTPErr removes the NUL terminator the TFTP library leaves on error messages sent by servers.
func cleanTFTPErr(err error) error {
	if err == nil || !strings.ContainsRune(err.Error(), 0) {
		return err
	}
	return errors.New(strings.TrimSpace(strings.ReplaceAll(err.Error(), "\x00", "")))
}

func (t *tftpClient) Get(ctx context.Context, remote string, w io.Writer, progress BytesFunc) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rf, err := t.c.Receive(remote, "octet")
	if err != nil {
		return cleanTFTPErr(err)
	}
	total := int64(-1)
	if in, ok := rf.(tftp.IncomingTransfer); ok {
		if n, ok := in.Size(); ok {
			total = n
		}
	}
	tr := newTracker(ctx, total, progress)
	_, err = rf.WriteTo(&trackedWriter{w: w, t: tr})
	tr.finish()
	return cleanTFTPErr(ctxErr(ctx, err))
}

func (t *tftpClient) Put(ctx context.Context, remote string, r io.Reader, size int64, progress BytesFunc) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	wt, err := t.c.Send(remote, "octet")
	if err != nil {
		return cleanTFTPErr(err)
	}
	if out, ok := wt.(tftp.OutgoingTransfer); ok && size >= 0 {
		out.SetSize(size)
	}
	tr := newTracker(ctx, size, progress)
	_, err = wt.ReadFrom(&trackedReader{r: r, t: tr})
	tr.finish()
	return cleanTFTPErr(ctxErr(ctx, err))
}
