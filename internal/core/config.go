package core

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

const configFileName = "config.json"

// ConfigDir returns the folder holding config.json, host keys and the default share.
// Portable by default: the folder of the executable. FILETRANS_HOME overrides it, and
// a user config directory is the fallback when the executable folder is read-only.
func ConfigDir() (string, error) {
	if d := Env("HOME"); d != "" {
		return d, os.MkdirAll(d, 0o755)
	}
	if exe, err := os.Executable(); err == nil {
		if parent, ok := appBundleParent(exe); ok {
			// A macOS app bundle must not be written into (it breaks its signature, and /Applications is
			// shared). Portable use there is opt-in: put a config.json next to the .app.
			if _, err := os.Stat(filepath.Join(parent, configFileName)); err == nil && writable(parent) {
				return parent, nil
			}
		} else if dir := filepath.Dir(exe); writable(dir) {
			return dir, nil
		}
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "filetrans")
	// Settings written under the app's earlier name stay in use rather than being left behind.
	if old := filepath.Join(base, "ftpapp"); !exists(dir) && exists(old) {
		return old, nil
	}
	return dir, os.MkdirAll(dir, 0o755)
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// Env reads the app's environment variable FILETRANS_<name>. The app used to be called FTP App, and
// FTPAPP_<name> is still honoured so existing shortcuts and scripts keep working.
func Env(name string) string {
	if v := os.Getenv("FILETRANS_" + name); v != "" {
		return v
	}
	return os.Getenv("FTPAPP_" + name)
}

// appBundleParent reports whether exe lives in a macOS bundle (Name.app/Contents/MacOS/exe) and, if so,
// returns the folder that holds the bundle.
func appBundleParent(exe string) (string, bool) {
	parts := strings.Split(filepath.ToSlash(filepath.Clean(exe)), "/")
	n := len(parts)
	if n < 4 || parts[n-2] != "MacOS" || parts[n-3] != "Contents" || !strings.HasSuffix(parts[n-4], ".app") {
		return "", false
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(exe)))), true
}

func writable(dir string) bool {
	f, err := os.CreateTemp(dir, ".w*")
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(f.Name())
	return true
}

// User is a login shared by the FTP and SFTP servers (TFTP has no authentication).
type User struct {
	Name         string `json:"name"`
	PasswordHash string `json:"passwordHash"`
	ReadOnly     bool   `json:"readOnly"`
}

// ServiceConfig holds the settings common to every server.
type ServiceConfig struct {
	AutoStart bool   `json:"autoStart"`
	BindAddr  string `json:"bindAddr"`
	Port      int    `json:"port"`
	Root      string `json:"root"`
	ReadOnly  bool   `json:"readOnly"` // refuse all writes regardless of user
}

type FTPConfig struct {
	ServiceConfig
	PassiveStart   int    `json:"passiveStart"` // 0 = let the OS pick passive ports
	PassiveEnd     int    `json:"passiveEnd"`
	PublicHost     string `json:"publicHost"` // IPv4 advertised in PASV replies, for use behind NAT
	AllowAnonymous bool   `json:"allowAnonymous"`
	TLS            bool   `json:"tls"`        // offer explicit FTPS (AUTH TLS) alongside plain FTP
	RequireTLS     bool   `json:"requireTls"` // refuse logins and transfers that are not encrypted
}

type SFTPConfig struct {
	ServiceConfig
}

type TFTPConfig struct {
	ServiceConfig
}

// Site is a saved client connection. Passwords are deliberately not stored: the config is a
// plain file next to the executable, so the UI asks for the password at connect time.
type Site struct {
	Name          string `json:"name"`
	Protocol      string `json:"protocol"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	User          string `json:"user"`
	KeyFile       string `json:"keyFile"`
	SkipTLSVerify bool   `json:"skipTlsVerify"`
	BlockSize     int    `json:"blockSize"` // TFTP only
	Parallel      int    `json:"parallel"`
}

type Config struct {
	Users []User     `json:"users"`
	FTP   FTPConfig  `json:"ftp"`
	SFTP  SFTPConfig `json:"sftp"`
	TFTP  TFTPConfig `json:"tftp"`
	Sites []Site     `json:"sites"`
}

// DefaultConfig returns safe defaults: unprivileged ports, anonymous access off and TFTP
// (which has no authentication) read-only and loopback-only until the user opts in.
func DefaultConfig(dir string) Config {
	share := "share" // relative to dir: the config stays valid when the folder is moved
	return Config{
		FTP: FTPConfig{
			ServiceConfig: ServiceConfig{BindAddr: "0.0.0.0", Port: 2121, Root: share},
			PassiveStart:  50000,
			PassiveEnd:    50100,
		},
		SFTP: SFTPConfig{ServiceConfig{BindAddr: "0.0.0.0", Port: 2222, Root: share}},
		TFTP: TFTPConfig{ServiceConfig{BindAddr: "127.0.0.1", Port: 6969, Root: share, ReadOnly: true}},
	}
}

// Validate checks values that would make a server fail in confusing ways at start-up.
func (c Config) Validate() error {
	seen := map[string]bool{}
	for _, u := range c.Users {
		n := strings.TrimSpace(u.Name)
		if n == "" {
			return errors.New("user name must not be empty")
		}
		if seen[strings.ToLower(n)] {
			return fmt.Errorf("duplicate user %q", n)
		}
		seen[strings.ToLower(n)] = true
	}
	sites := map[string]bool{}
	for _, s := range c.Sites {
		n := strings.ToLower(strings.TrimSpace(s.Name))
		if n == "" {
			return errors.New("site name must not be empty")
		}
		if sites[n] {
			return fmt.Errorf("duplicate site %q", s.Name)
		}
		sites[n] = true
		p := ConnectParams{Protocol: s.Protocol, Host: s.Host, Port: s.Port}
		if err := p.validate(); err != nil {
			return fmt.Errorf("site %q: %w", s.Name, err)
		}
	}
	for name, s := range map[string]ServiceConfig{"FTP": c.FTP.ServiceConfig, "SFTP": c.SFTP.ServiceConfig, "TFTP": c.TFTP.ServiceConfig} {
		if s.Port < 0 || s.Port > 65535 {
			return fmt.Errorf("%s: port %d out of range", name, s.Port)
		}
		if s.BindAddr != "" && net.ParseIP(s.BindAddr) == nil {
			return fmt.Errorf("%s: %q is not an IP address", name, s.BindAddr)
		}
		if strings.TrimSpace(s.Root) == "" {
			return fmt.Errorf("%s: root folder must not be empty", name)
		}
		if err := checkRootSyntax(s.Root); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if c.FTP.PassiveStart != 0 || c.FTP.PassiveEnd != 0 {
		if c.FTP.PassiveStart < 1 || c.FTP.PassiveEnd > 65535 || c.FTP.PassiveStart > c.FTP.PassiveEnd {
			return errors.New("FTP: invalid passive port range")
		}
	}
	if c.FTP.RequireTLS && !c.FTP.TLS {
		return errors.New("FTP: \"require TLS\" needs TLS to be enabled")
	}
	if h := c.FTP.PublicHost; h != "" {
		if ip := net.ParseIP(h); ip == nil || ip.To4() == nil {
			return fmt.Errorf("FTP: public host %q must be an IPv4 address", h)
		}
	}
	return nil
}

// LoadConfig reads dir/config.json. When it does not exist yet, it creates a default
// config with one random-password user "admin" and returns that password (otherwise "").
func LoadConfig(dir string) (cfg Config, initialPassword string, err error) {
	p := filepath.Join(dir, configFileName)
	data, err := os.ReadFile(p)
	switch {
	case err == nil:
		cfg = DefaultConfig(dir)
		if err := json.Unmarshal(data, &cfg); err != nil {
			return cfg, "", fmt.Errorf("%s: %w", p, err)
		}
		if cfg.normalizeRoots(dir) {
			// An older config stored full paths; rewrite them so the folder can be moved. Best effort:
			// the in-memory config is right either way.
			SaveConfig(dir, cfg)
		}
		return cfg, "", cfg.Validate()
	case errors.Is(err, os.ErrNotExist):
		cfg = DefaultConfig(dir)
		initialPassword, err = randomPassword(12)
		if err != nil {
			return cfg, "", err
		}
		if err := cfg.SetUser("admin", initialPassword, false); err != nil {
			return cfg, "", err
		}
		return cfg, initialPassword, SaveConfig(dir, cfg)
	default:
		return cfg, "", err
	}
}

// SaveConfig writes the config atomically so a crash cannot leave a half-written file.
func SaveConfig(dir string, cfg Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(dir, configFileName)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// SetUser adds or updates a user. An empty password keeps the existing hash of an existing user.
func (c *Config) SetUser(name, password string, readOnly bool) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("user name must not be empty")
	}
	for i := range c.Users {
		if strings.EqualFold(c.Users[i].Name, name) {
			c.Users[i].ReadOnly = readOnly
			if password != "" {
				h, err := hashPassword(password)
				if err != nil {
					return err
				}
				c.Users[i].PasswordHash = h
			}
			return nil
		}
	}
	if password == "" {
		return errors.New("password must not be empty for a new user")
	}
	h, err := hashPassword(password)
	if err != nil {
		return err
	}
	c.Users = append(c.Users, User{Name: name, PasswordHash: h, ReadOnly: readOnly})
	return nil
}

func (c *Config) DeleteUser(name string) {
	out := c.Users[:0:0]
	for _, u := range c.Users {
		if !strings.EqualFold(u.Name, name) {
			out = append(out, u)
		}
	}
	c.Users = out
}

func hashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(h), err
}

// dummyHash is compared against for unknown users so login timing does not reveal which names exist.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("dummy"), bcrypt.DefaultCost)

// userDB is an immutable snapshot of the users taken when a server starts.
type userDB map[string]User

func newUserDB(users []User) userDB {
	db := make(userDB, len(users))
	for _, u := range users {
		db[strings.ToLower(u.Name)] = u
	}
	return db
}

func (db userDB) Authenticate(name, password string) (User, bool) {
	u, ok := db[strings.ToLower(name)]
	hash := []byte(u.PasswordHash)
	if !ok {
		hash = dummyHash
	}
	if bcrypt.CompareHashAndPassword(hash, []byte(password)) != nil || !ok {
		return User{}, false
	}
	return u, true
}

const passwordAlphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func randomPassword(n int) (string, error) {
	b := make([]byte, n)
	max := big.NewInt(int64(len(passwordAlphabet)))
	for i := range b {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b[i] = passwordAlphabet[v.Int64()]
	}
	return string(b), nil
}
