# File Trans

A small portable desktop app with **FTP / FTPS / SFTP / TFTP servers** and a **client** for all four.
Go backend, plain HTML/JS frontend in a native window ([Wails](https://wails.io), uses the system WebView).
One executable; settings live in `config.json` next to it (copy the folder to move everything).

## Using it

- **Servers tab** – start/stop FTP, SFTP and TFTP, choose port, shared folder and options.
  On first run an `admin` user is created and its password is shown once; manage users at the bottom.
  *Server activity* shows what clients are transferring right now (bytes, speed, time left) and the last
  finished transfers. The Log tab records long transfers every few seconds and every finish with size and duration.
  A server knows the size of a *download* up front, so it can show time left; for an *upload* the client does not
  announce the size (except TFTP clients that send `tsize`), so only bytes received, speed and elapsed time are shown.
- **Client tab** – connect (FTP, FTPS, SFTP with password or key file, TFTP), browse both sides,
  select files and use *Upload* / *Download* or drag between the panels. Folders transfer recursively;
  every transfer runs on its own connection and can be cancelled. Each transfer shows percent, speed and
  the estimated time left (for a folder, across all its files), and how long it took when finished.
- **Log tab** – live activity of servers and transfers.

Speed: FTP, FTPS and SFTP are faster than most networks (measured on loopback through the running app:
FTP about 1.5 GB/s down and 0.5-1 GB/s up, SFTP about 330 MB/s down and 210 MB/s up), and neither the
memory savings below nor the UI slow them down. TFTP sends one block at a time, so its speed follows the block
size: 512 bytes gives about 25 MB/s, 1468 about 80 MB/s, 8192 about 400 MB/s. The client asks for 1468 by
default (fits one Ethernet frame); the *Block size* field on the Client tab raises it on a clean LAN. The server
grants whatever size a client asks for, up to 16384.

Parallel transfers: the client moves several files at once, each on its own connection from a per-session
pool (default 4, set it with *Parallel transfers* on the Client tab). It applies to the files inside a folder
and to many selected items alike, and the pool never opens more connections than that number. Measured with
200 files of 64 KB: over SFTP on loopback 657 ms with one connection, 245 ms with eight; over a link with 20 ms
latency 20.4 s with one connection and 2.9 s with eight. One big file is not split, and big files already
saturate a disk, so they gain little. If a server refuses extra connections (many limit them per user), the
pool notices, keeps working with the connections it has and does not fail the transfer.

Defaults are conservative: ports 2121 / 2222 / 6969 (no admin rights needed), no anonymous access, and
TFTP (no login) is read-only and loopback-only until you change it.

Memory (Windows): the UI runs in WebView2, whose helper processes are most of the footprint. The app turns off
GPU compositing (about 45 MB less; set `FILETRANS_GPU=1` to turn it back on) and hands unused memory back to Windows
when the window has been hidden or untouched for a while, never during a transfer. Measured with Task Manager's
"Memory" figure: about 80 MB right after opening, around 10 MB a few seconds later, about 20 MB while idle and
about 30 MB in use; the Go process itself needs only about 8 MB. Pages that are needed again come back in a few
milliseconds. Linux and macOS leave the web view to the system.

Moving the app: copy the whole folder (the `.exe`, `config.json`, the host key and the `share` folder) and it
works from the new place, because shared folders are stored relative to the app's folder (`"root": "share"`).
A folder picked inside the app folder is stored that way automatically, and configs from older versions that
hold full paths are converted on the next start. A folder elsewhere stays a full path and has to exist on the
new machine. The shared folder may not contain the app's own folder, because `config.json` holds password hashes
and the SSH host key is a private key.

Where the settings go: the folder of the executable, or `FILETRANS_HOME` if set, or the user config
folder when the executable folder is read-only.

## Building

Requirements: Go 1.26+, Node.js, and the Wails CLI (`go install github.com/wailsapp/wails/v2/cmd/wails@latest`).
On Linux also `libgtk-3-dev` and `libwebkit2gtk-4.1-dev`; on Windows the WebView2 runtime (present on Windows 11).

```
wails build            # build for the current OS -> build/bin
wails dev              # live development; the UI is also served at http://localhost:34115
go test ./...          # protocol and logic tests (servers are exercised with real clients)
```

Cross-platform targets: `windows/amd64`, `linux/amd64`, `linux/arm64`, `darwin/universal`. Wails needs the
system web view, so each target has to be built on its own OS. `.github/workflows/build.yml` does that on GitHub
Actions (Actions tab > build > Run workflow, or push a tag such as `v1.0.0` to also publish a release); each run
produces a zip/tar.gz per system and runs the tests on every one of them.
On Ubuntu/Debian build with `wails build -tags webkit2_41` (needs `libwebkit2gtk-4.1-dev`).

Linux: unpack, `chmod +x FileTrans`, run. Needs GTK 3 and `libwebkit2gtk-4.1` (Ubuntu 22.04+, Debian 12+, Fedora).
macOS: the build is unsigned. After downloading, run `xattr -cr FileTrans.app` once (or right-click > Open) to get
past Gatekeeper. Inside an `.app` the app does not write next to itself; settings go to
`~/Library/Application Support/filetrans`. For a portable setup copy a `config.json` from another install next to the
`.app`; when one is there, that folder is used instead (including its `share` folder).

## Layout

- `internal/core` – everything protocol-related, independent of the UI: servers (`ftp.go`, `sftp.go`, `tftp.go`),
  clients (`client*.go`), transfer helpers (`transfer.go`), config, sandboxed file access (`fs.go`), host-key store.
- `app.go` – the methods exposed to the frontend and the transfer queue.
- `frontend/src` – the UI (`servers.js`, `client.js`, `log.js`, `util.js`).

## Security notes

- Shared folders are confined with `os.Root`: `..` and symlinks/junctions cannot escape.
- Names received from a server are never used as paths or HTML without validation.
- SFTP host keys: the first connection to a server shows its key fingerprint and asks before trusting it
  (nothing is sent to the server until you accept). A later change of a trusted key is refused until you
  forget the host.
- Saved sites never store passwords. User passwords are stored as bcrypt hashes.
- Password guessing: 8 wrong passwords from one address within 10 minutes block that address for 5 minutes, on
  FTP and SFTP together (they share the users). At most 64 connections per address and 256 in total are accepted.
  Passwords set in the app need at least 8 characters.
- A server does not start when its shared folder contains the settings folder (directly or through a link),
  because clients could then download `config.json` and the private keys.
- A TFTP upload that breaks off leaves the previous file untouched (the data goes to `<name>.tftp-part` first).
- The page runs under a Content-Security-Policy that allows only the app's own scripts.
- Plain FTP sends passwords unencrypted, and FTP and SFTP share the same users. On a network you do not fully
  trust, use SFTP, or turn on TLS and *Require TLS* for FTP.
- FTPS uses a self-signed certificate (`ftps_cert.pem` / `ftps_key.pem`); replace the files to use your own.
