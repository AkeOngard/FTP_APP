package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ServiceStatus is what the UI shows for each server.
type ServiceStatus struct {
	Name    string `json:"name"`
	Running bool   `json:"running"`
	Addr    string `json:"addr"`
}

// Manager owns the configuration, the three servers and the open client sessions.
type Manager struct {
	dir   string
	log   *Logger
	mon   *Monitor
	known *KnownHosts
	guard *loginGuard

	mu              sync.Mutex
	cfg             Config
	services        map[string]Service
	sessions        map[string]*session
	initialPassword string
}

// session is an open client connection plus what is needed to open further connections
// to the same server (transfers use their own so a long one does not block browsing).
type session struct {
	c    RemoteClient
	p    ConnectParams
	pool *ConnPool // transfer connections; created on first use
}

// NewManager loads (or creates) the config in dir. On first run it generates a password for
// the "admin" user, which the UI fetches once through InitialPassword.
func NewManager(dir string) (*Manager, error) {
	cfg, initialPassword, err := LoadConfig(dir)
	if err != nil {
		return nil, err
	}
	log := NewLogger()
	known, err := LoadKnownHosts(dir, log)
	if err != nil {
		return nil, err
	}
	m := &Manager{
		dir: dir, log: log, mon: NewMonitor(log), known: known, cfg: cfg,
		services: map[string]Service{},
		sessions: map[string]*session{},

		initialPassword: initialPassword,
	}
	m.guard = newLoginGuard()
	m.guard.onBlock = func(addr string, d time.Duration) {
		m.log.Errorf("app", "%s: too many failed logins, blocked for %s", addr, HumanDuration(d))
	}
	if initialPassword != "" {
		m.log.Infof("app", "first run: created user \"admin\" (password shown once on the Servers tab)")
	}
	return m, nil
}

// InitialPassword returns the generated admin password on the first call after the first
// run, then "" so it is shown exactly once and never kept in the log.
func (m *Manager) InitialPassword() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	pw := m.initialPassword
	m.initialPassword = ""
	return pw
}

func (m *Manager) Dir() string             { return m.dir }
func (m *Manager) Log() *Logger            { return m.log }
func (m *Manager) KnownHosts() *KnownHosts { return m.known }
func (m *Manager) Monitor() *Monitor       { return m.mon }
func (m *Manager) Config() Config          { m.mu.Lock(); defer m.mu.Unlock(); return m.cfg.clone() }

func (c Config) clone() Config {
	c.Users = append([]User(nil), c.Users...)
	c.Sites = append([]Site(nil), c.Sites...)
	return c
}

// update applies mutate to a copy of the config, validates and saves it, and only then adopts it.
func (m *Manager) update(mutate func(*Config) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.cfg.clone()
	if err := mutate(&next); err != nil {
		return err
	}
	next.normalizeRoots(m.dir) // a folder picked inside the app folder is stored relative, so it moves with it
	if err := next.Validate(); err != nil {
		return err
	}
	if err := next.checkRootsAvoidSettings(m.dir); err != nil {
		return err
	}
	if err := SaveConfig(m.dir, next); err != nil {
		return err
	}
	m.cfg = next
	return nil
}

// SetConfig validates, stores and saves cfg. Running servers keep their old settings
// until they are restarted.
func (m *Manager) SetConfig(cfg Config) error {
	return m.update(func(c *Config) error { *c = cfg.clone(); return nil })
}

// SetUser adds or updates a user and saves. An empty password keeps the current one.
func (m *Manager) SetUser(name, password string, readOnly bool) error {
	if password != "" && len(password) < minPasswordLen {
		return fmt.Errorf("the password must have at least %d characters", minPasswordLen)
	}
	return m.update(func(c *Config) error { return c.SetUser(name, password, readOnly) })
}

func (m *Manager) DeleteUser(name string) error {
	return m.update(func(c *Config) error { c.DeleteUser(name); return nil })
}

// SaveSite adds or replaces a saved connection (matched by name, case-insensitively).
func (m *Manager) SaveSite(s Site) error {
	return m.update(func(c *Config) error {
		for i := range c.Sites {
			if strings.EqualFold(c.Sites[i].Name, s.Name) {
				c.Sites[i] = s
				return nil
			}
		}
		c.Sites = append(c.Sites, s)
		return nil
	})
}

func (m *Manager) DeleteSite(name string) error {
	return m.update(func(c *Config) error {
		out := c.Sites[:0:0]
		for _, s := range c.Sites {
			if !strings.EqualFold(s.Name, name) {
				out = append(out, s)
			}
		}
		c.Sites = out
		return nil
	})
}

// minPasswordLen applies to passwords set from the UI. The servers are reachable from the network and
// a short password falls to guessing even with the login guard.
const minPasswordLen = 8

var serviceNames = []string{ServiceFTP, ServiceSFTP, ServiceTFTP}

// currentUsers snapshots the users at the moment of a login, so password changes and
// deletions take effect without restarting a server.
func (m *Manager) currentUsers() userDB {
	m.mu.Lock()
	defer m.mu.Unlock()
	return newUserDB(m.cfg.Users)
}

// ResolveRoot returns the absolute path of a configured shared folder (relative ones are relative to
// the app's folder).
func (m *Manager) ResolveRoot(root string) string { return ResolveRoot(m.dir, root) }

func (m *Manager) build(name string) (Service, error) {
	users := m.currentUsers
	// The servers always get absolute paths, resolved now against the folder the app runs from.
	// Saving already refuses a share that exposes the settings; checking again here covers configs
	// written by older versions or edited by hand, and folders that became links since.
	for n, root := range map[string]string{ServiceFTP: m.cfg.FTP.Root, ServiceSFTP: m.cfg.SFTP.Root, ServiceTFTP: m.cfg.TFTP.Root} {
		if n == name {
			if err := checkRootAvoidsSettings(m.dir, m.ResolveRoot(root)); err != nil {
				return nil, err
			}
		}
	}
	switch name {
	case ServiceFTP:
		cfg := m.cfg.FTP
		cfg.Root = m.ResolveRoot(cfg.Root)
		s := newFTPService(cfg, users, m.log, m.mon, m.dir)
		s.guard = m.guard
		return s, nil
	case ServiceSFTP:
		cfg := m.cfg.SFTP
		cfg.Root = m.ResolveRoot(cfg.Root)
		s := newSFTPService(cfg, users, m.log, m.mon, m.dir)
		s.guard = m.guard
		return s, nil
	case ServiceTFTP:
		cfg := m.cfg.TFTP
		cfg.Root = m.ResolveRoot(cfg.Root)
		return newTFTPService(cfg, m.log, m.mon), nil
	}
	return nil, fmt.Errorf("unknown service %q", name)
}

// Start launches a server from the current config. It is an error if it is already running.
func (m *Manager) Start(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.services[name]; ok && s.Running() {
		return errors.New(name + " is already running")
	}
	s, err := m.build(name)
	if err != nil {
		m.log.Errorf(name, "start failed: %v", err)
		return err
	}
	if err := s.Start(); err != nil {
		m.log.Errorf(name, "start failed: %v", err)
		return err
	}
	m.services[name] = s
	return nil
}

func (m *Manager) Stop(name string) error {
	m.mu.Lock()
	s, ok := m.services[name]
	delete(m.services, name)
	m.mu.Unlock()
	if !ok {
		return nil
	}
	return s.Stop()
}

// Restart applies config changes to a running server.
func (m *Manager) Restart(name string) error {
	if err := m.Stop(name); err != nil {
		return err
	}
	return m.Start(name)
}

// StartAutoStart launches every server flagged AutoStart, logging failures.
func (m *Manager) StartAutoStart() {
	cfg := m.Config()
	auto := map[string]bool{
		ServiceFTP:  cfg.FTP.AutoStart,
		ServiceSFTP: cfg.SFTP.AutoStart,
		ServiceTFTP: cfg.TFTP.AutoStart,
	}
	for _, n := range serviceNames {
		if auto[n] {
			m.Start(n) // failure is already logged
		}
	}
}

func (m *Manager) StopAll() {
	for _, n := range serviceNames {
		m.Stop(n)
	}
}

func (m *Manager) Status() []ServiceStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ServiceStatus, 0, len(serviceNames))
	for _, n := range serviceNames {
		st := ServiceStatus{Name: n}
		if s, ok := m.services[n]; ok && s.Running() {
			st.Running, st.Addr = true, s.Addr()
		}
		out = append(out, st)
	}
	return out
}

// Connect opens a client session and returns its id.
func (m *Manager) Connect(ctx context.Context, p ConnectParams) (string, error) {
	c, err := Connect(ctx, p, m.known)
	if err != nil {
		var unknown *HostKeyUnknownError
		if errors.As(err, &unknown) {
			return "", err // not a failure: the user is asked to confirm the key, then it connects again
		}
		m.log.Errorf(p.Protocol, "connect %s failed: %v", p.Host, err)
		return "", err
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		c.Close()
		return "", err
	}
	id := hex.EncodeToString(b[:])
	m.mu.Lock()
	m.sessions[id] = &session{c: c, p: p}
	m.mu.Unlock()
	m.log.Infof(p.Protocol, "connected to %s", p.Host)
	return id, nil
}

func (m *Manager) session(id string) (*session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("no such session %q", id)
	}
	return s, nil
}

// Client returns the open session with the given id.
func (m *Manager) Client(id string) (RemoteClient, error) {
	s, err := m.session(id)
	if err != nil {
		return nil, err
	}
	return s.c, nil
}

// Dial opens a new, independent connection to the server of an existing session. The caller closes it.
func (m *Manager) Dial(ctx context.Context, id string) (RemoteClient, error) {
	s, err := m.session(id)
	if err != nil {
		return nil, err
	}
	return Connect(ctx, s.p, m.known)
}

// Pool returns the session's pool of transfer connections, created on first use. It holds up to the
// session's Parallel connections, separate from the one used for browsing, so a long transfer never
// stalls the file lists. The pool is closed with the session.
func (m *Manager) Pool(id string) (*ConnPool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("no such session %q", id)
	}
	if s.pool == nil {
		p, known := s.p, m.known
		// Pooled connections outlive any single transfer, so they must not be tied to a transfer's context.
		s.pool = NewConnPool(func(context.Context) (RemoteClient, error) {
			return Connect(context.Background(), p, known)
		}, p.parallel())
	}
	return s.pool, nil
}

func (m *Manager) Disconnect(id string) error {
	m.mu.Lock()
	s, ok := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	if !ok {
		return nil
	}
	if s.pool != nil {
		s.pool.Close()
	}
	return s.c.Close()
}

// Shutdown stops every server and closes every client session.
func (m *Manager) Shutdown() {
	m.StopAll()
	m.mu.Lock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.Disconnect(id)
	}
}
