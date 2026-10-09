package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
)

const knownHostsFileName = "known_hosts.json"

// HostKeyChangedError means a server presented a different key than the one trusted earlier,
// which can indicate a man-in-the-middle attack or a reinstalled server.
type HostKeyChangedError struct {
	Host, Trusted, Presented string
}

func (e *HostKeyChangedError) Error() string {
	return fmt.Sprintf("host key for %s changed (trusted %s, server presented %s); "+
		"if the server was reinstalled, forget the host and connect again", e.Host, e.Trusted, e.Presented)
}

// HostKeyUnknownError means the server is not trusted yet. Nothing has been sent to it: the caller shows
// Key to the user and, once they accept it, calls Trust and connects again.
type HostKeyUnknownError struct {
	Host string // "host:port"
	Key  string // "key-type SHA256:fingerprint"
}

func (e *HostKeyUnknownError) Error() string {
	return fmt.Sprintf("the host key of %s is not trusted yet (%s)", e.Host, e.Key)
}

// KnownHosts remembers SFTP host keys. A host seen for the first time is refused with a
// HostKeyUnknownError until its key has been accepted through Trust, and any later change of a trusted
// key is refused.
type KnownHosts struct {
	path string
	log  *Logger

	// TrustNew accepts and records unknown hosts without asking (trust on first use). For unattended
	// use and tests; the app leaves it off and asks the user.
	TrustNew bool

	mu    sync.Mutex
	hosts map[string]string // "host:port" -> "key-type SHA256:fingerprint"
}

// LoadKnownHosts reads dir/known_hosts.json; a missing file is an empty list.
func LoadKnownHosts(dir string, log *Logger) (*KnownHosts, error) {
	k := &KnownHosts{path: filepath.Join(dir, knownHostsFileName), log: log, hosts: map[string]string{}}
	data, err := os.ReadFile(k.path)
	if errors.Is(err, os.ErrNotExist) {
		return k, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &k.hosts); err != nil {
		return nil, fmt.Errorf("%s: %w", k.path, err)
	}
	return k, nil
}

func hostKeyID(key ssh.PublicKey) string {
	return key.Type() + " " + ssh.FingerprintSHA256(key)
}

func normalizeHost(h string) string { return strings.ToLower(h) }

// Callback returns the ssh.HostKeyCallback implementing the policy.
func (k *KnownHosts) Callback() ssh.HostKeyCallback {
	return func(hostname string, _ net.Addr, key ssh.PublicKey) error {
		host, got := normalizeHost(hostname), hostKeyID(key)
		k.mu.Lock()
		defer k.mu.Unlock()
		trusted, ok := k.hosts[host]
		if ok {
			if trusted != got {
				return &HostKeyChangedError{Host: hostname, Trusted: trusted, Presented: got}
			}
			return nil
		}
		if !k.TrustNew {
			return &HostKeyUnknownError{Host: hostname, Key: got}
		}
		return k.trustLocked(hostname, got)
	}
}

// Trust records key (as reported by a HostKeyUnknownError) as the trusted key of hostname ("host:port").
// The next connection is accepted only if the server presents exactly that key. A host that already has
// a different trusted key is not overwritten: forget it first.
func (k *KnownHosts) Trust(hostname, key string) error {
	keyType, fp, ok := strings.Cut(key, " ")
	if strings.TrimSpace(hostname) == "" || !ok || keyType == "" || !strings.HasPrefix(fp, "SHA256:") {
		return errors.New("not a host key fingerprint")
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if trusted, ok := k.hosts[normalizeHost(hostname)]; ok {
		if trusted != key {
			return &HostKeyChangedError{Host: hostname, Trusted: trusted, Presented: key}
		}
		return nil
	}
	return k.trustLocked(hostname, key)
}

func (k *KnownHosts) trustLocked(hostname, key string) error {
	host := normalizeHost(hostname)
	k.hosts[host] = key
	if err := k.saveLocked(); err != nil {
		delete(k.hosts, host)
		return fmt.Errorf("cannot record host key: %w", err)
	}
	if k.log != nil {
		k.log.Infof("sftp", "trusting new host key for %s: %s", hostname, key)
	}
	return nil
}

// algorithms returns the host key algorithms to offer for host: once a key type is trusted,
// only that type is accepted, so a server offering several keys cannot trigger false mismatches.
func (k *KnownHosts) algorithms(hostname string) []string {
	k.mu.Lock()
	trusted, ok := k.hosts[normalizeHost(hostname)]
	k.mu.Unlock()
	if !ok {
		return nil
	}
	keyType, _, _ := strings.Cut(trusted, " ")
	if keyType == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
	}
	return []string{keyType}
}

// Forget removes the trusted key of host ("host:port") so the next connection re-learns it.
func (k *KnownHosts) Forget(hostname string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.hosts, normalizeHost(hostname))
	return k.saveLocked()
}

// List returns a copy of the trusted keys.
func (k *KnownHosts) List() map[string]string {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make(map[string]string, len(k.hosts))
	for h, v := range k.hosts {
		out[h] = v
	}
	return out
}

func (k *KnownHosts) saveLocked() error {
	data, err := json.MarshalIndent(k.hosts, "", "  ")
	if err != nil {
		return err
	}
	tmp := k.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, k.path)
}
