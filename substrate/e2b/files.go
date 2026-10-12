package e2b

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"connectrpc.com/connect"

	"github.com/jrimmer/spoond/v3/substrate"
	"github.com/jrimmer/spoond/v3/substrate/e2b/gen/envd/filesystem"
	"github.com/jrimmer/spoond/v3/substrate/e2b/gen/envd/filesystem/filesystemconnect"
)

// WriteFile writes data to path via envd's HTTP POST /files (multipart).
//
// envd sets no mode and opens an existing file with O_CREAT as root after
// chowning it to the guest user. In a sticky world-writable directory
// (/tmp) under fs.protected_regular=1 — systemd's default, so Arch-based
// images — the kernel refuses root that open of another user's file, so
// the upload can never land on a file that already exists there. So the
// upload goes to a fresh name in a private 0700 directory beside the
// target (envd creates it new; a secret is never readable by others while
// its contents land), and one exec then sets the mode and renames it into
// place: rename is atomic and is not subject to protected_regular. All
// execs pass the paths as positional arguments, never interpolated.
func (c *Client) WriteFile(ctx context.Context, sandboxID, path string, data []byte, mode os.FileMode) error {
	if mode == 0 {
		mode = 0o644
	}
	dir, base := splitPath(path)
	if base == "" {
		return fmt.Errorf("e2b: write %s: %w", path, substrate.ErrInvalidOp)
	}
	tmpDir, err := uploadDir(dir)
	if err != nil {
		return fmt.Errorf("e2b: write %s: %w", path, err)
	}
	tmp := joinPath(tmpDir, base)
	if err := c.shExec(ctx, sandboxID, "stage "+path, writeStageScript, path, dir, tmpDir); err != nil {
		return err
	}
	if err := uploadFile(ctx, c.envdClient(sandboxID, ""), c.cfg.ProxyURL, tmp, data); err != nil {
		c.discard(ctx, sandboxID, tmpDir)
		if serr := c.sandboxListed(ctx, sandboxID); serr != nil {
			return fmt.Errorf("%w: %v", serr, err)
		}
		return err
	}
	if err := c.shExec(ctx, sandboxID, "place "+path, writePlaceScript, strconv.FormatUint(uint64(mode.Perm()), 8), tmp, path, tmpDir); err != nil {
		c.discard(ctx, sandboxID, tmpDir)
		return err
	}
	return nil
}

// writeStageScript refuses a directory target, creates the target's
// missing parents (0755) and the private upload directory (0700).
// $1 target, $2 its directory, $3 the upload directory.
const writeStageScript = `set -e
[ ! -d "$1" ] || { echo "$1: is a directory" >&2; exit 1; }
mkdir -p -- "$2"
mkdir -m 700 -- "$3"`

// writePlaceScript applies the mode, renames the upload over the target
// and removes the upload directory. $1 mode, $2 upload, $3 target, $4 the
// upload directory.
const writePlaceScript = `set -e
chmod "$1" -- "$2"
mv -f -- "$2" "$3"
rmdir -- "$4"`

// shExec runs a fixed /bin/sh script with args as its positional
// parameters ($1…): the script is a constant, so no path ever reaches the
// shell as code.
func (c *Client) shExec(ctx context.Context, sandboxID, what, script string, args ...string) error {
	r, err := c.Exec(ctx, sandboxID, substrate.ExecRequest{
		Args: append([]string{"/bin/sh", "-c", script, "sh"}, args...),
	})
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return fmt.Errorf("e2b: %s %s: exit %d: %s", what, sandboxID, r.ExitCode, strings.TrimSpace(r.Stderr))
	}
	return nil
}

// discard removes a failed write's upload directory, best effort: the
// write's own error is what the caller reports.
func (c *Client) discard(ctx context.Context, sandboxID, tmpDir string) {
	_, _ = c.Exec(ctx, sandboxID, substrate.ExecRequest{Args: []string{"/bin/rm", "-rf", "--", tmpDir}})
}

// uploadDir names a fresh private upload directory inside dir.
func uploadDir(dir string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return joinPath(dir, ".spoond-upload-"+hex.EncodeToString(b[:])), nil
}

// splitPath splits an absolute guest path into its directory and base
// name ("/f" is "/" and "f"; a trailing slash leaves an empty base).
func splitPath(path string) (dir, base string) {
	i := strings.LastIndexByte(path, '/')
	if i < 0 {
		return ".", path
	}
	if i == 0 {
		return "/", path[1:]
	}
	return path[:i], path[i+1:]
}

func joinPath(dir, name string) string {
	if strings.HasSuffix(dir, "/") {
		return dir + name
	}
	return dir + "/" + name
}

// ReadFile downloads path via envd's HTTP GET /files; the error wraps
// substrate.ErrNotFound for a missing path and substrate.ErrTooLarge when the
// file is larger than max bytes.
func (c *Client) ReadFile(ctx context.Context, sandboxID, path string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, filesURL(c.cfg.ProxyURL, path), nil)
	if err != nil {
		return nil, fmt.Errorf("e2b: read %s %s: %w", sandboxID, path, err)
	}
	resp, err := c.envdClient(sandboxID, "").Do(req)
	if err != nil {
		return nil, fmt.Errorf("e2b: read %s %s: %w", sandboxID, path, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		// read below
	case http.StatusNotFound:
		return nil, fmt.Errorf("e2b: read %s %s: %w", sandboxID, path, substrate.ErrNotFound)
	default:
		return nil, fmt.Errorf("e2b: read %s %s: unexpected status %s", sandboxID, path, resp.Status)
	}
	// max+1 so an over-size file is detectable without reading it all; the
	// clamps keep the arithmetic sane at the extremes.
	limit := max
	if limit < 0 {
		limit = 0
	}
	if limit < math.MaxInt64 {
		limit++
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, fmt.Errorf("e2b: read %s %s: %w", sandboxID, path, err)
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("e2b: read %s %s: %w", sandboxID, path, substrate.ErrTooLarge)
	}
	return data, nil
}

// ReadFileRange reads at most limit bytes of path starting at offset,
// via envd's HTTP /files endpoint with a Range header. A missing path
// wraps substrate.ErrNotFound; an offset past the end yields no bytes.
// Used by the background-job output endpoints (2.6, #135) so a client
// can follow output without downloading the whole file.
func (c *Client) ReadFileRange(ctx context.Context, sandboxID, path string, offset, limit int64) ([]byte, error) {
	if offset < 0 {
		offset = 0
	}
	if limit < 0 {
		limit = 0
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, filesURL(c.cfg.ProxyURL, path), nil)
	if err != nil {
		return nil, fmt.Errorf("e2b: read range %s %s: %w", sandboxID, path, err)
	}
	if limit > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+limit-1))
	} else {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := c.envdClient(sandboxID, "").Do(req)
	if err != nil {
		return nil, fmt.Errorf("e2b: read range %s %s: %w", sandboxID, path, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		// The server ignored the Range header: skip the prefix itself.
		if offset > 0 {
			if _, err := io.CopyN(io.Discard, resp.Body, offset); err != nil && err != io.EOF {
				return nil, fmt.Errorf("e2b: read range %s %s: %w", sandboxID, path, err)
			}
		}
	case http.StatusPartialContent:
		// read below
	case http.StatusRequestedRangeNotSatisfiable:
		return nil, nil
	case http.StatusNotFound:
		return nil, fmt.Errorf("e2b: read range %s %s: %w", sandboxID, path, substrate.ErrNotFound)
	default:
		return nil, fmt.Errorf("e2b: read range %s %s: unexpected status %s", sandboxID, path, resp.Status)
	}
	// limit+1 so an over-size answer is detectable without reading it all.
	readLimit := limit
	if readLimit < math.MaxInt64 {
		readLimit++
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, readLimit))
	if err != nil {
		return nil, fmt.Errorf("e2b: read range %s %s: %w", sandboxID, path, err)
	}
	if int64(len(data)) > limit {
		data = data[:limit]
	}
	return data, nil
}

// Stat reports path's metadata through envd's filesystem.Stat.
func (c *Client) Stat(ctx context.Context, sandboxID, path string) (substrate.FileInfo, error) {
	resp, err := c.envdFilesystem(sandboxID, "").Stat(ctx, connect.NewRequest(&filesystem.StatRequest{Path: path}))
	if err != nil {
		return substrate.FileInfo{}, fmt.Errorf("e2b: stat %s %s: %w", sandboxID, path, mapConnectError(err))
	}
	return entryInfo(resp.Msg.GetEntry()), nil
}

// MakeDir creates path and any missing parents through envd's
// filesystem.MakeDir, then applies mode with a chmod exec (MakeDirRequest
// carries no mode).
func (c *Client) MakeDir(ctx context.Context, sandboxID, path string, mode os.FileMode) error {
	if mode == 0 {
		mode = 0o755
	}
	if _, err := c.envdFilesystem(sandboxID, "").MakeDir(ctx, connect.NewRequest(&filesystem.MakeDirRequest{Path: path})); err != nil {
		return fmt.Errorf("e2b: mkdir %s %s: %w", sandboxID, path, mapConnectError(err))
	}
	return c.chmod(ctx, sandboxID, path, mode)
}

// Rename moves oldPath to newPath, replacing newPath, through envd's
// filesystem.Move. It backs the atomic guest-file writes (2.7, #83).
func (c *Client) Rename(ctx context.Context, sandboxID, oldPath, newPath string) error {
	if _, err := c.envdFilesystem(sandboxID, "").Move(ctx, connect.NewRequest(&filesystem.MoveRequest{
		Source:      oldPath,
		Destination: newPath,
	})); err != nil {
		return fmt.Errorf("e2b: rename %s %s -> %s: %w", sandboxID, oldPath, newPath, mapConnectError(err))
	}
	return nil
}

// Remove deletes path. Recursive removes go through envd's filesystem.Remove,
// which deletes directories with all their contents. A non-recursive remove
// keeps POSIX semantics without recursion: files go through filesystem.Remove
// (it deletes a single file), directories through exec rmdir, which refuses a
// non-empty directory, so recursive=false never destroys contents.
func (c *Client) Remove(ctx context.Context, sandboxID, path string, recursive bool) error {
	fs := c.envdFilesystem(sandboxID, "")
	if recursive {
		if _, err := fs.Remove(ctx, connect.NewRequest(&filesystem.RemoveRequest{Path: path})); err != nil {
			return fmt.Errorf("e2b: remove %s %s: %w", sandboxID, path, mapConnectError(err))
		}
		return nil
	}
	info, err := c.Stat(ctx, sandboxID, path)
	if err != nil {
		return err
	}
	if !info.IsDir {
		if _, err := fs.Remove(ctx, connect.NewRequest(&filesystem.RemoveRequest{Path: path})); err != nil {
			return fmt.Errorf("e2b: remove %s %s: %w", sandboxID, path, mapConnectError(err))
		}
		return nil
	}
	r, err := c.Exec(ctx, sandboxID, substrate.ExecRequest{
		Args: []string{"/bin/rmdir", "--", path},
	})
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return rmdirError(sandboxID, path, r.ExitCode, r.Stderr)
	}
	return nil
}

// rmdirError maps a failed rmdir to the substrate's errors: "Directory
// not empty" (coreutils and busybox both say it) is ErrNotEmpty, which
// the files API answers with 409; anything else stays a plain failure.
func rmdirError(sandboxID, path string, exit int, stderr string) error {
	msg := strings.TrimSpace(stderr)
	if strings.Contains(strings.ToLower(msg), "not empty") {
		return fmt.Errorf("e2b: remove %s %s: %w", sandboxID, path, substrate.ErrNotEmpty)
	}
	return fmt.Errorf("e2b: remove %s %s: exit %d: %s", sandboxID, path, exit, msg)
}

// envdFilesystem returns the Connect client for one sandbox's envd filesystem
// service, with the same routing and auth headers the exec path uses.
func (c *Client) envdFilesystem(sandboxID, user string) filesystemconnect.FilesystemClient {
	h := envdHeaders{sandboxID: sandboxID, token: c.EnvdToken(sandboxID), user: defaultUser(user)}
	hc := &http.Client{Transport: &envdTransport{base: c.http.Transport, h: h}}
	return filesystemconnect.NewFilesystemClient(hc, c.cfg.ProxyURL, connect.WithInterceptors(&h))
}

// chmod applies mode to path with a guest chmod (envd's file APIs set no
// mode themselves).
func (c *Client) chmod(ctx context.Context, sandboxID, path string, mode os.FileMode) error {
	r, err := c.Exec(ctx, sandboxID, substrate.ExecRequest{
		Args: []string{"/bin/chmod", strconv.FormatUint(uint64(mode.Perm()), 8), "--", path},
	})
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return fmt.Errorf("e2b: chmod %s %s: exit %d: %s", sandboxID, path, r.ExitCode, strings.TrimSpace(r.Stderr))
	}
	return nil
}

// entryInfo converts an envd EntryInfo.
func entryInfo(e *filesystem.EntryInfo) substrate.FileInfo {
	mode := os.FileMode(e.GetMode() & 0o777)
	switch e.GetType() {
	case filesystem.FileType_FILE_TYPE_DIRECTORY:
		mode |= os.ModeDir
	case filesystem.FileType_FILE_TYPE_SYMLINK:
		mode |= os.ModeSymlink
	}
	return substrate.FileInfo{
		Name:    e.GetName(),
		Size:    e.GetSize(),
		Mode:    mode,
		ModTime: e.GetModifiedTime().AsTime(),
		IsDir:   e.GetType() == filesystem.FileType_FILE_TYPE_DIRECTORY,
	}
}

// uploadFile POSTs one multipart form part named "file" to envd's /files
// endpoint; envd creates the file's parent directories and replaces an
// existing file.
func uploadFile(ctx context.Context, hc *http.Client, baseURL, path string, data []byte) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", baseName(path))
	if err != nil {
		return fmt.Errorf("e2b: upload %s: %w", path, err)
	}
	if _, err := part.Write(data); err != nil {
		return fmt.Errorf("e2b: upload %s: %w", path, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("e2b: upload %s: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, filesURL(baseURL, path), &buf)
	if err != nil {
		return fmt.Errorf("e2b: upload %s: %w", path, err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("e2b: upload %s: %w", path, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("e2b: upload %s: %w", path, substrate.ErrNotFound)
	default:
		return fmt.Errorf("e2b: upload %s: unexpected status %s", path, resp.Status)
	}
}

// filesURL builds envd's /files URL with the path query parameter.
func filesURL(baseURL, path string) string {
	return strings.TrimSuffix(baseURL, "/") + "/files?path=" + url.QueryEscape(path)
}

func baseName(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}

// mapConnectError converts envd Connect errors to substrate sentinels.
func mapConnectError(err error) error {
	if connect.CodeOf(err) == connect.CodeNotFound {
		return fmt.Errorf("%w: %v", substrate.ErrNotFound, err)
	}
	return err
}
