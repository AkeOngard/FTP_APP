package core

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
)

// A server seen for the first time is not trusted by itself: the connection is refused with the key,
// and only the key the user accepted is trusted afterwards.
func TestUnknownSFTPHostNeedsConfirmation(t *testing.T) {
	m, params := startAllServers(t, nil)
	m.KnownHosts().TrustNew = false // as the app runs
	p := params[ProtoSFTP]
	hostPort := net.JoinHostPort(p.Host, strconv.Itoa(p.Port))

	_, err := m.Connect(context.Background(), p)
	var unknown *HostKeyUnknownError
	if !errors.As(err, &unknown) {
		t.Fatalf("first contact: got %v, want HostKeyUnknownError", err)
	}
	if unknown.Host != hostPort || !strings.Contains(unknown.Key, " SHA256:") {
		t.Errorf("prompt data: %+v", unknown)
	}
	if len(m.KnownHosts().List()) != 0 {
		t.Error("the host was recorded before anyone accepted it")
	}
	if strings.Contains(joined(m.Log().Entries()), "failed") {
		t.Errorf("asking for confirmation was logged as a failure: %s", joined(m.Log().Entries()))
	}

	// Declined: asking again gives the same answer, still nothing recorded.
	if _, err := m.Connect(context.Background(), p); !errors.As(err, &unknown) {
		t.Fatalf("second contact without trust: %v", err)
	}

	// A key other than the one the server has (what an interceptor's would be) must not get through.
	fake := strings.Fields(unknown.Key)[0] + " SHA256:" + strings.Repeat("A", 43)
	if err := m.KnownHosts().Trust(hostPort, fake); err != nil {
		t.Fatal(err)
	}
	var changed *HostKeyChangedError
	if _, err := m.Connect(context.Background(), p); !errors.As(err, &changed) {
		t.Fatalf("connected although the trusted key differs from the server's: %v", err)
	}
	// ...and accepting the real key now does not silently replace the trusted one.
	if err := m.KnownHosts().Trust(hostPort, unknown.Key); !errors.As(err, &changed) {
		t.Errorf("Trust replaced a different trusted key: %v", err)
	}
	if err := m.KnownHosts().Forget(hostPort); err != nil {
		t.Fatal(err)
	}

	// Accepted: connects, and stays trusted after a restart of the app.
	if err := m.KnownHosts().Trust(hostPort, unknown.Key); err != nil {
		t.Fatal(err)
	}
	id, err := m.Connect(context.Background(), p)
	if err != nil {
		t.Fatalf("after accepting the key: %v", err)
	}
	// transfer connections of the session are opened later, without another question
	pool, _ := m.Pool(id)
	c, err := pool.Get(context.Background())
	if err != nil {
		t.Fatalf("pooled connection: %v", err)
	}
	pool.Put(c, false)
	m.Disconnect(id)
	reloaded, err := LoadKnownHosts(m.Dir(), nil)
	if err != nil || reloaded.List()[hostPort] != unknown.Key {
		t.Errorf("accepted key not persisted: %v %v", reloaded.List(), err)
	}

	for _, bad := range []string{"", "ssh-ed25519", "ssh-ed25519 MD5:aa:bb", " SHA256:abc"} {
		if err := m.KnownHosts().Trust("example:22", bad); err == nil {
			t.Errorf("Trust accepted %q as a fingerprint", bad)
		}
	}
	if err := m.KnownHosts().Trust(" ", unknown.Key); err == nil {
		t.Error("Trust accepted an empty host")
	}
}
